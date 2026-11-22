package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/otel"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

type otelStatusShape struct {
	Enabled       bool             `json:"enabled"`
	Endpoint      string           `json:"endpoint"`
	ServiceName   string           `json:"service_name"`
	SpansExported int64            `json:"spans_exported"`
	SpansDropped  int64            `json:"spans_dropped"`
	ExportErrors  int64            `json:"export_errors"`
	LastError     string           `json:"last_error"`
	QueuedSpans   int              `json:"queued_spans"`
	Counters      map[string]int64 `json:"counters"`
}

func setupOtelAPI(t *testing.T, tr *otel.Tracer) (*httptest.Server, string, string) {
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
	rest.SetTracer(tr)
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	login := func(username, password string) string {
		body := strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`)
		resp, err := http.Post(srv.URL+"/api/login", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Session string `json:"session"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Session
	}
	return srv, login("admin", "agentgate-admin"), login("lixiaofeng", "demo12345")
}

func getOtel(t *testing.T, srv *httptest.Server, sess string) (int, otelStatusShape) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/otel", nil)
	req.Header.Set("Authorization", "Bearer "+sess)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got otelStatusShape
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp.StatusCode, got
}

func TestOtelStatusDisabled(t *testing.T) {
	srv, admin, emp := setupOtelAPI(t, nil)
	s, got := getOtel(t, srv, admin)
	if s != 200 || got.Enabled {
		t.Fatalf("nil tracer must report disabled, got %d %+v", s, got)
	}
	if got.ServiceName != "agentgate" {
		t.Fatalf("default service name must be agentgate, got %+v", got)
	}
	s, _ = getOtel(t, srv, emp)
	if s != 403 {
		t.Fatalf("employee want 403, got %d", s)
	}
}

func TestOtelTestEndpoint(t *testing.T) {
	var mu sync.Mutex
	var traceBodies [][]byte
	col := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		if r.URL.Path == "/v1/traces" {
			traceBodies = append(traceBodies, body)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer col.Close()

	tr := otel.New(otel.Config{Endpoint: col.URL, ServiceName: "agentgate-test", ServiceVersion: "test"})
	srv, admin, _ := setupOtelAPI(t, tr)

	// The test endpoint emits a synthetic span and flushes it.
	req, _ := http.NewRequest("POST", srv.URL+"/api/otel/test", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("test endpoint want 200, got %d", resp.StatusCode)
	}
	s, got := getOtel(t, srv, admin)
	if s != 200 || got.SpansExported < 1 {
		t.Fatalf("want spans_exported >= 1, got %d %+v", s, got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(traceBodies) != 1 || !strings.Contains(string(traceBodies[0]), "agentgate.test") {
		t.Fatalf("collector must receive the test span, got %d bodies", len(traceBodies))
	}
}

func TestOtelTestEndpointDisabled(t *testing.T) {
	srv, admin, _ := setupOtelAPI(t, otel.New(otel.Config{}))
	req, _ := http.NewRequest("POST", srv.URL+"/api/otel/test", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("disabled tracer test want 503, got %d", resp.StatusCode)
	}
}

func TestOtelTestEndpointCollectorDown(t *testing.T) {
	// Point at a dead port: the export fails, the endpoint reports
	// 502 and the failure is counted in status.
	tr := otel.New(otel.Config{Endpoint: "http://127.0.0.1:9", ServiceName: "agentgate-test"})
	srv, admin, _ := setupOtelAPI(t, tr)
	req, _ := http.NewRequest("POST", srv.URL+"/api/otel/test", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("dead collector test want 502, got %d", resp.StatusCode)
	}
	_, got := getOtel(t, srv, admin)
	if got.ExportErrors < 1 || got.LastError == "" {
		t.Fatalf("failed export must be counted: %+v", got)
	}
}
