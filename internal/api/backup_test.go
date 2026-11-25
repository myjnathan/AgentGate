package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/backup"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

// fakeS3Server is a minimal S3 stand-in for the backup API tests:
// it accepts any PUT, answers LIST with an empty bucket, and answers
// GET with 404. The backup client unit tests cover the protocol;
// here only the API wiring (config storage, run trigger, masking)
// is under test.
func fakeS3Server(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	stored := map[string][]byte{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated></ListBucketResult>`))
			return
		}
		switch r.Method {
		case http.MethodPut:
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r.Body)
			stored[r.URL.Path] = buf.Bytes()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if b, ok := stored[r.URL.Path]; ok {
				_, _ = w.Write(b)
				return
			}
			http.Error(w, "not found", http.StatusNotFound)
		case http.MethodDelete:
			delete(stored, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func setupBackupAPI(t *testing.T, s3URL string) (*httptest.Server, string, string, *store.Store, *secretbox.Box) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secretbox.New("test-master-key-123456")
	au := auth.New(st)
	if err := seed.Demo(st, box, "http://127.0.0.1:9"); err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rest := api.New(st, au, box, al)
	if s3URL != "" {
		cfg := backup.Config{Endpoint: s3URL, Bucket: "bkt", Region: "us-east-1",
			AccessKey: "env-ak", SecretKey: "env-sk"}.WithDefaults()
		svc := backup.NewService(dir,
			func() (backup.Config, error) {
				// Same precedence as cmd/agentgate: a stored
				// console config wins, environment is the fallback.
				var stored *store.BackupConfig
				st.Read(func(s *store.State) { stored = s.Backup })
				if stored != nil && stored.Endpoint != "" {
					secret, err := box.Decrypt(stored.SecretEnc)
					if err != nil {
						return backup.Config{}, err
					}
					c := backup.Config{Endpoint: stored.Endpoint, Bucket: stored.Bucket,
						Region: stored.Region, AccessKey: stored.AccessKey,
						SecretKey: secret, Source: "stored"}.WithDefaults()
					return c, c.Validate()
				}
				return cfg, nil
			},
			st.SnapshotBytes,
			func(ev audit.Event) { al.Append(ev) },
			func(rep backup.Report) {
				// Same persistence as cmd/agentgate: the last run
				// is recorded on the stored config for the status
				// page (an env-only setup gets a bare record).
				st.WithLock(func(s *store.State) {
					if s.Backup == nil {
						s.Backup = &store.BackupConfig{}
					}
					s.Backup.LastRun = &store.BackupRunSummary{
						StartedAt: rep.StartedAt, FinishedAt: rep.FinishedAt,
						StateUploaded: rep.StateUploaded, StateKey: rep.StateKey,
						AuditUploaded: len(rep.AuditUploaded), AuditSkipped: rep.AuditSkipped,
						Pruned: len(rep.Pruned), Error: rep.Error,
					}
				})
				_ = st.Save()
			}, nil)
		rest.SetBackupService(svc)
	}
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, loginAs(t, srv.URL, "admin", "agentgate-admin"), loginAs(t, srv.URL, "lixiaofeng", "demo12345"), st, box
}

func callBackup(t *testing.T, srv *httptest.Server, sess, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+sess)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestBackupStatusUnconfigured(t *testing.T) {
	srv, admin, employee, _, _ := setupBackupAPI(t, "")
	s, body := callBackup(t, srv, admin, "GET", "/api/backup/status", "")
	if s != 200 || body["configured"] != false {
		t.Fatalf("status = %d %+v, want 200 configured:false", s, body)
	}
	s, _ = callBackup(t, srv, admin, "POST", "/api/backup/run", "{}")
	if s != 503 {
		t.Fatalf("run without service = %d, want 503", s)
	}
	s, _ = callBackup(t, srv, employee, "GET", "/api/backup/status", "")
	if s != 403 {
		t.Fatalf("employee status = %d, want 403", s)
	}
}

func TestBackupConfigAndRun(t *testing.T) {
	s3 := fakeS3Server(t)
	defer s3.Close()
	srv, admin, employee, st, _ := setupBackupAPI(t, s3.URL)

	// Config validation first: a bad endpoint is refused.
	s, _ := callBackup(t, srv, admin, "POST", "/api/backup/config",
		`{"endpoint":"ftp://x","bucket":"bkt","access_key":"ak","secret_key":"sk-secret-1"}`)
	if s != 400 {
		t.Fatalf("bad endpoint config = %d, want 400", s)
	}
	s, _ = callBackup(t, srv, admin, "POST", "/api/backup/config",
		`{"endpoint":"http://127.0.0.1:9","bucket":"bkt","access_key":"ak"}`)
	if s != 400 {
		t.Fatalf("missing secret config = %d, want 400", s)
	}

	// Store a config: the response must mask the access key and
	// never contain the secret.
	s, body := callBackup(t, srv, admin, "POST", "/api/backup/config",
		`{"endpoint":"`+s3.URL+`","bucket":"agentgate-backup","access_key":"AKIAIOSFODNN7EXAMPLE","secret_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}`)
	if s != 200 {
		t.Fatalf("config = %d %+v", s, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "wJalrXUtnFEMI") {
		t.Fatalf("config response leaks the secret: %s", raw)
	}
	if body["access_key_masked"] != "****MPLE" || body["has_secret"] != true || body["source"] != "stored" {
		t.Fatalf("config response = %+v, want masked key + has_secret + stored", body)
	}
	// The secret is encrypted at rest, not plaintext in state.json.
	var stored *store.BackupConfig
	st.Read(func(s *store.State) { stored = s.Backup })
	if stored == nil || stored.SecretEnc == "" || strings.Contains(stored.SecretEnc, "wJalrXUtnFEMI") {
		t.Fatalf("stored secret not encrypted: %+v", stored)
	}

	// Status now reports the stored config; a run uploads the
	// state snapshot to the fake bucket.
	s, body = callBackup(t, srv, admin, "GET", "/api/backup/status", "")
	if s != 200 || body["configured"] != true || body["bucket"] != "agentgate-backup" {
		t.Fatalf("status = %d %+v", s, body)
	}
	s, body = callBackup(t, srv, admin, "POST", "/api/backup/run", "{}")
	if s != 200 {
		t.Fatalf("run = %d %+v", s, body)
	}
	if body["state_uploaded"] != true || body["state_key"] == nil || body["state_key"] == "" {
		t.Fatalf("run report = %+v, want state_uploaded", body)
	}
	s, body = callBackup(t, srv, admin, "GET", "/api/backup/status", "")
	if s != 200 {
		t.Fatalf("status after run = %d", s)
	}
	lastRun, ok := body["last_run"].(map[string]any)
	if !ok || lastRun["state_uploaded"] != true {
		t.Fatalf("status last_run = %+v, want state_uploaded", body["last_run"])
	}

	// Employees may not configure or run backups.
	s, _ = callBackup(t, srv, employee, "POST", "/api/backup/config",
		`{"endpoint":"`+s3.URL+`","bucket":"bkt","access_key":"ak","secret_key":"sk"}`)
	if s != 403 {
		t.Fatalf("employee config = %d, want 403", s)
	}
	s, _ = callBackup(t, srv, employee, "POST", "/api/backup/run", "{}")
	if s != 403 {
		t.Fatalf("employee run = %d, want 403", s)
	}
}
