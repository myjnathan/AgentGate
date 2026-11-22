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

type sidecarStatsShape struct {
	SidecarCalls     int64  `json:"sidecar_calls"`
	SidecarCacheHits int64  `json:"sidecar_cache_hits"`
	LastSeen         string `json:"last_seen"`
	ByAgent          []struct {
		AgentType string `json:"agent_type"`
		Calls     int64  `json:"calls"`
	} `json:"by_agent"`
}

func TestSidecarStatsEndpoint(t *testing.T) {
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
	// Seed audit events directly: two sidecar-routed model calls, one
	// sidecar cache hit, one direct call, one sidecar violation.
	now := time.Now().UTC()
	al.Append(audit.Event{Actor: "lixiaofeng", ActorID: "u1", AgentType: "dsh", Kind: "model_call",
		Action: "chat_completions", Model: "mock-eco", Decision: "allow",
		Detail: map[string]string{"sidecar": "ag-guard/0.1.0"}, Time: now.Add(-time.Hour)})
	al.Append(audit.Event{Actor: "lixiaofeng", ActorID: "u1", AgentType: "dsh", Kind: "model_call",
		Action: "chat_completions", Model: "mock-eco", Decision: "allow",
		Detail: map[string]string{"sidecar": "ag-guard/0.1.0"}, Time: now})
	al.Append(audit.Event{Actor: "lixiaofeng", ActorID: "u1", AgentType: "dsh", Kind: "cache_hit",
		Action: "chat_completions", Model: "mock-eco",
		Detail: map[string]string{"cache": "hit", "sidecar": "ag-guard/0.1.0"}, Time: now})
	al.Append(audit.Event{Actor: "lixiaofeng", ActorID: "u1", AgentType: "dsh", Kind: "model_call",
		Action: "chat_completions", Model: "mock-eco", Decision: "allow", Time: now})
	al.Append(audit.Event{Actor: "lixiaofeng", ActorID: "u1", AgentType: "dsh", Kind: "violation",
		Action: "model_call_blocked", Decision: "block",
		Detail: map[string]string{"sidecar": "ag-guard/0.1.0"}, Time: now})

	rest := api.New(st, au, box, al)
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
	admin := login("admin", "agentgate-admin")

	req, _ := http.NewRequest("GET", srv.URL+"/api/sidecar/stats", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got sidecarStatsShape
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// Two sidecar model calls + one sidecar cache hit; the direct
	// call and the violation do not count.
	if got.SidecarCalls != 2 {
		t.Fatalf("want sidecar_calls 2, got %+v", got)
	}
	if got.SidecarCacheHits != 1 {
		t.Fatalf("want sidecar_cache_hits 1, got %+v", got)
	}
	if got.LastSeen == "" {
		t.Fatalf("want last_seen set, got %+v", got)
	}
	if len(got.ByAgent) != 1 || got.ByAgent[0].AgentType != "dsh" || got.ByAgent[0].Calls != 2 {
		t.Fatalf("want by_agent [{dsh 2}], got %+v", got.ByAgent)
	}

	// Employees cannot read sidecar stats.
	emp := login("lixiaofeng", "demo12345")
	req, _ = http.NewRequest("GET", srv.URL+"/api/sidecar/stats", nil)
	req.Header.Set("Authorization", "Bearer "+emp)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("employee want 403, got %d", resp.StatusCode)
	}
}
