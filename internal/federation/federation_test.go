package federation_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// fakeUpstream is a minimal streamable-HTTP MCP server: tools/list
// advertises two tools, tools/call echoes its arguments and reports
// whether the bearer token arrived.
type fakeUpstream struct {
	hits     atomic.Int32
	authSeen atomic.Int32
	failList bool
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Method == "tools/list" {
		f.hits.Add(1)
		if f.failList {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		writeResult(w, map[string]any{"tools": []map[string]any{
			{"name": "leave_balance", "description": "remaining leave days",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			{"name": "project_status", "description": "project status",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
		}})
		return
	}
	if req.Method == "tools/call" {
		if r.Header.Get("Authorization") == "Bearer upstream-secret" {
			f.authSeen.Add(1)
		}
		text, _ := json.Marshal(map[string]any{"tool": req.Params.Name, "args": req.Params.Arguments})
		writeResult(w, map[string]any{"content": []map[string]any{
			{"type": "text", "text": string(text)}}, "isError": false})
		return
	}
	http.Error(w, "unknown method", http.StatusNotFound)
}

func writeResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func setupRegistry(t *testing.T, fake *fakeUpstream) (*federation.Registry, *store.UpstreamServer) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New("test-master-key-123456")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	enc, err := box.Encrypt("upstream-secret")
	if err != nil {
		t.Fatal(err)
	}
	up := &store.UpstreamServer{ID: "up_test", Name: "hr", URL: srv.URL,
		AuthTokenEnc: enc, HasToken: true, MaxDataLevel: store.LevelInternal,
		MinTier: store.TierBYOA, Enabled: true, CreatedAt: time.Now()}
	st.WithLock(func(s *store.State) { s.Upstreams[up.ID] = up })
	return federation.New(st, box), up
}

func TestListToolsCachesWithinTTL(t *testing.T) {
	fake := &fakeUpstream{}
	reg, up := setupRegistry(t, fake)
	ctx := context.Background()
	if _, err := reg.ListTools(ctx, up); err != nil {
		t.Fatalf("first ListTools: %v", err)
	}
	tools, err := reg.ListTools(ctx, up)
	if err != nil {
		t.Fatalf("second ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "leave_balance" {
		t.Fatalf("tools = %+v, want leave_balance + project_status", tools)
	}
	if fake.hits.Load() != 1 {
		t.Fatalf("upstream tools/list hits = %d, want 1 (second call served from cache)", fake.hits.Load())
	}
}

func TestListToolsFallsBackToStaleCache(t *testing.T) {
	old := federation.CacheTTL
	federation.CacheTTL = time.Millisecond
	t.Cleanup(func() { federation.CacheTTL = old })
	fake := &fakeUpstream{}
	reg, up := setupRegistry(t, fake)
	ctx := context.Background()
	if _, err := reg.ListTools(ctx, up); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	fake.failList = true
	time.Sleep(2 * time.Millisecond) // let the warmed entry go stale
	// A fresh fetch now fails, but the stale entry still answers.
	tools, err := reg.ListTools(ctx, up)
	if err != nil || len(tools) != 2 {
		t.Fatalf("ListTools with stale cache: tools=%+v err=%v", tools, err)
	}
	// The explicit fresh check still reports the live failure.
	if _, err := reg.ListToolsFresh(ctx, up); err == nil {
		t.Fatal("ListToolsFresh on a failing upstream should error")
	}
}

func TestCallToolForwardsAuthAndArgs(t *testing.T) {
	fake := &fakeUpstream{}
	reg, up := setupRegistry(t, fake)
	text, isErr, err := reg.CallTool(context.Background(), up, "leave_balance",
		map[string]any{"employee": "lixiaofeng"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if isErr {
		t.Fatalf("CallTool isError = true, want false")
	}
	var got struct {
		Tool string         `json:"tool"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("CallTool result is not JSON: %v (%s)", err, text)
	}
	if got.Tool != "leave_balance" || got.Args["employee"] != "lixiaofeng" {
		t.Fatalf("forwarded call = %+v, want leave_balance/lixiaofeng", got)
	}
	if fake.authSeen.Load() != 1 {
		t.Fatalf("upstream saw the bearer token %d times, want 1", fake.authSeen.Load())
	}
}

func TestCallToolUpstreamHTTPError(t *testing.T) {
	reg, up := setupRegistry(t, &fakeUpstream{})
	// Point at a dead port: connection refused must surface as an error.
	up.URL = "http://127.0.0.1:9/mcp"
	if _, _, err := reg.CallTool(context.Background(), up, "leave_balance", nil); err == nil {
		t.Fatal("CallTool against a dead upstream should error")
	}
	if _, err := reg.ListTools(context.Background(), up); err == nil {
		t.Fatal("ListTools against a dead upstream (cold cache) should error")
	}
}
