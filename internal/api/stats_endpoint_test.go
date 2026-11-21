package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

// statsShape mirrors the wire shape of GET /api/stats for assertions.
type statsShape struct {
	Totals struct {
		Calls   int64 `json:"calls"`
		Tokens  int64 `json:"tokens"`
		Blocked int64 `json:"blocked"`
	} `json:"totals"`
	ByUser       []bucket `json:"by_user"`
	ByDepartment []bucket `json:"by_department"`
	ByChannel    []bucket `json:"by_channel"`
	ByModel      []bucket `json:"by_model"`
	ByAgentType  []bucket `json:"by_agent_type"`
	ByDataLevel  []bucket `json:"by_data_level"`
	ByDecision   []bucket `json:"by_decision"`
}

type bucket struct {
	Key     string `json:"key"`
	Calls   int64  `json:"calls"`
	Tokens  int64  `json:"tokens"`
	Blocked int64  `json:"blocked"`
}

func findBucket(bs []bucket, key string) *bucket {
	for i := range bs {
		if bs[i].Key == key {
			return &bs[i]
		}
	}
	return nil
}

// TestStatsEndpoint covers aggregation through the real handler:
// totals, every dimension, auth gating, and the time window.
func TestStatsEndpoint(t *testing.T) {
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
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	login := func(username, password string) (string, int) {
		body := strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`)
		resp, err := http.Post(srv.URL+"/api/login", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d struct {
			Session string `json:"session"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&d)
		return d.Session, resp.StatusCode
	}
	adminSess, code := login("admin", "agentgate-admin")
	if code != 200 || adminSess == "" {
		t.Fatalf("admin login failed: %d", code)
	}
	empSess, code := login("lixiaofeng", "demo12345")
	if code != 200 || empSess == "" {
		t.Fatalf("employee login failed: %d", code)
	}

	var adminID, liID string
	st.Read(func(s *store.State) {
		for _, u := range s.Users {
			switch u.Username {
			case "admin":
				adminID = u.ID
			case "lixiaofeng":
				liID = u.ID
			}
		}
	})

	// Three model calls + one blocked call + out-of-scope noise.
	al.Append(audit.Event{Kind: "model_call", Action: "chat_completions", Actor: "lixiaofeng", ActorID: liID,
		AgentType: "claude-code", Model: "mock-eco", Channel: "eco", DataLevel: "internal", Decision: "allow", Tokens: 100})
	al.Append(audit.Event{Kind: "model_call", Action: "chat_completions", Actor: "lixiaofeng", ActorID: liID,
		AgentType: "claude-code", Model: "mock-eco", Channel: "eco", DataLevel: "internal", Decision: "redact", Tokens: 50})
	al.Append(audit.Event{Kind: "model_call", Action: "chat_completions", Actor: "admin", ActorID: adminID,
		AgentType: "dsh", Model: "mock-pro", Channel: "pro", DataLevel: "confidential", Decision: "reroute", Tokens: 200})
	al.Append(audit.Event{Kind: "violation", Action: "model_call_blocked", Actor: "lixiaofeng", ActorID: liID,
		AgentType: "claude-code", Model: "mock-pro", DataLevel: "confidential", Decision: "block"})
	al.Append(audit.Event{Kind: "auth", Action: "login", Actor: "lixiaofeng", ActorID: liID, Decision: "allow"})
	al.Append(audit.Event{Kind: "channel", Action: "channel_cooldown", Actor: "system", Channel: "eco", Decision: "cooldown"})

	get := func(path, sess string) (int, statsShape) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var s statsShape
		_ = json.NewDecoder(resp.Body).Decode(&s)
		return resp.StatusCode, s
	}

	code, s := get("/api/stats", adminSess)
	if code != 200 {
		t.Fatalf("GET /api/stats = %d, want 200", code)
	}
	if s.Totals.Calls != 4 || s.Totals.Tokens != 350 || s.Totals.Blocked != 1 {
		t.Fatalf("totals = %+v, want calls=4 tokens=350 blocked=1", s.Totals)
	}
	// Department dimension resolves the actor's department from the store.
	if b := findBucket(s.ByDepartment, "研发部"); b == nil || b.Calls != 3 || b.Tokens != 150 || b.Blocked != 1 {
		t.Fatalf("by_department[研发部] = %+v, want calls=3 tokens=150 blocked=1", b)
	}
	if b := findBucket(s.ByDepartment, "IT"); b == nil || b.Calls != 1 || b.Tokens != 200 {
		t.Fatalf("by_department[IT] = %+v, want calls=1 tokens=200", b)
	}
	if b := findBucket(s.ByUser, "lixiaofeng"); b == nil || b.Calls != 3 {
		t.Fatalf("by_user[lixiaofeng] = %+v, want calls=3", b)
	}
	if b := findBucket(s.ByChannel, "eco"); b == nil || b.Calls != 2 || b.Tokens != 150 {
		t.Fatalf("by_channel[eco] = %+v, want calls=2 tokens=150", b)
	}
	// Blocked calls have no channel: they land in the "none" bucket.
	if b := findBucket(s.ByChannel, "none"); b == nil || b.Calls != 1 || b.Blocked != 1 {
		t.Fatalf("by_channel[none] = %+v, want calls=1 blocked=1", b)
	}
	if b := findBucket(s.ByModel, "mock-eco"); b == nil || b.Calls != 2 {
		t.Fatalf("by_model[mock-eco] = %+v, want calls=2", b)
	}
	if b := findBucket(s.ByAgentType, "claude-code"); b == nil || b.Calls != 3 {
		t.Fatalf("by_agent_type[claude-code] = %+v, want calls=3", b)
	}
	if b := findBucket(s.ByDataLevel, "confidential"); b == nil || b.Calls != 2 || b.Blocked != 1 {
		t.Fatalf("by_data_level[confidential] = %+v, want calls=2 blocked=1", b)
	}
	if b := findBucket(s.ByDecision, "allow"); b == nil || b.Calls != 1 {
		t.Fatalf("by_decision[allow] = %+v, want calls=1", b)
	}
	if b := findBucket(s.ByDecision, "block"); b == nil || b.Calls != 1 {
		t.Fatalf("by_decision[block] = %+v, want calls=1", b)
	}
	if b := findBucket(s.ByDecision, "redact"); b == nil || b.Calls != 1 {
		t.Fatalf("by_decision[redact] = %+v, want calls=1", b)
	}
	if b := findBucket(s.ByDecision, "reroute"); b == nil || b.Calls != 1 {
		t.Fatalf("by_decision[reroute] = %+v, want calls=1", b)
	}

	// Time window: everything happened just now, so a future window is empty.
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	code, s = get("/api/stats?since="+future, adminSess)
	if code != 200 || s.Totals.Calls != 0 {
		t.Fatalf("future window: code=%d totals=%+v, want 200 and 0 calls", code, s.Totals)
	}

	// Bad time format is a 400, not a silent ignore.
	code, _ = get("/api/stats?since=not-a-date", adminSess)
	if code != 400 {
		t.Fatalf("bad since: code=%d, want 400", code)
	}
	code, _ = get("/api/stats?since=2026-12-01T00:00:00Z&until=2026-11-01T00:00:00Z", adminSess)
	if code != 400 {
		t.Fatalf("until<=since: code=%d, want 400", code)
	}

	// Gating: employee (non-admin) is forbidden, anonymous is unauthorized.
	code, _ = get("/api/stats", empSess)
	if code != 403 {
		t.Fatalf("employee stats: code=%d, want 403", code)
	}
	code, _ = get("/api/stats", "")
	if code != 401 {
		t.Fatalf("anonymous stats: code=%d, want 401", code)
	}
}
