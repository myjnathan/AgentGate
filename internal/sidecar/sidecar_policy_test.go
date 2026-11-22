package sidecar_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/sidecar"
)

// startSidecarServer builds a sidecar daemon against the fake
// gateway and returns both the daemon (for StartBackground /
// ConfigHash) and its loopback server.
func startSidecarServer(t *testing.T, gw *fakeGateway, cfg sidecar.Config) (*sidecar.Server, *httptest.Server) {
	t.Helper()
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)
	cfg.GatewayURL = gwSrv.URL
	srv, err := sidecar.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	s := httptest.NewServer(srv.Handler())
	t.Cleanup(s.Close)
	return srv, s
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func sidecarStatus(t *testing.T, base string) sidecar.Status {
	t.Helper()
	resp, err := http.Get(base + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st sidecar.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPolicyRefreshUpdatesDetector(t *testing.T) {
	gw := &fakeGateway{policyBanned: []string{"new_banned"}}
	srv, s := startSidecarServer(t, gw, sidecar.Config{BannedWords: []string{"old_banned"}})
	before := srv.ConfigHash()

	// The first chat triggers the background refresh; the request
	// itself is still judged by the startup word list.
	if code, _ := postChat(t, s.URL, "tok", "mock-eco", "包含 old_banned 内容"); code != 400 {
		t.Fatalf("old banned word must block before refresh, got %d", code)
	}
	waitFor(t, "policy refresh", func() bool {
		return sidecarStatus(t, s.URL).PolicySource == "gateway"
	})
	if got := sidecarStatus(t, s.URL).PolicyVersion; got == "" {
		t.Fatalf("refreshed policy must carry a version")
	}
	if srv.ConfigHash() == before {
		t.Fatalf("config hash must change when the policy refreshes")
	}
	// The new word list is now enforced; the old one is gone.
	if code, _ := postChat(t, s.URL, "tok", "mock-eco", "包含 new_banned 内容"); code != 400 {
		t.Fatalf("refreshed banned word must block, got %d", code)
	}
	if code, _ := postChat(t, s.URL, "tok", "mock-eco", "包含 old_banned 内容"); code != 200 {
		t.Fatalf("retired banned word must pass after refresh, got %d", code)
	}
	if gw.policyHitCount() == 0 {
		t.Fatalf("want at least one policy fetch")
	}
}

func TestPolicyCacheSurvivesRestart(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "policy-cache.json")
	gw := &fakeGateway{policyBanned: []string{"cached_banned"}}
	srv, s := startSidecarServer(t, gw, sidecar.Config{PolicyCachePath: cache})
	srv.StartBackground()
	if code, _ := postChat(t, s.URL, "tok", "mock-eco", "hello"); code != 200 {
		t.Fatalf("seed chat: got %d", code)
	}
	waitFor(t, "policy refresh", func() bool {
		return sidecarStatus(t, s.URL).PolicySource == "gateway"
	})
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("policy cache must be written: %v", err)
	}
	wantVersion := sidecarStatus(t, s.URL).PolicyVersion

	// A fresh daemon against a dead gateway loads the cache and
	// enforces it: the cached banned word still blocks locally.
	srv2, err := sidecar.New(sidecar.Config{
		GatewayURL: "http://127.0.0.1:1", PolicyCachePath: cache,
		HeartbeatIntervalSecs: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv2.StartBackground()
	s2 := httptest.NewServer(srv2.Handler())
	t.Cleanup(s2.Close)
	st := sidecarStatus(t, s2.URL)
	if st.PolicySource != "cached" || st.PolicyVersion != wantVersion {
		t.Fatalf("want cached policy %s, got %+v", wantVersion, st)
	}
	if code, body := postChat(t, s2.URL, "tok", "mock-eco", "包含 cached_banned 内容"); code != 400 || !strings.Contains(body, "sidecar_blocked") {
		t.Fatalf("cached banned word must block offline, got %d %s", code, body)
	}
}

func TestPolicyCacheTamperedIsIgnored(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "policy-cache.json")
	if err := os.WriteFile(cache, []byte(`{"version":"deadbeef","banned_words":["x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := sidecar.New(sidecar.Config{
		GatewayURL: "http://127.0.0.1:1", PolicyCachePath: cache,
		BannedWords: []string{"startup_banned"}, HeartbeatIntervalSecs: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.StartBackground()
	s := httptest.NewServer(srv.Handler())
	t.Cleanup(s.Close)
	if got := sidecarStatus(t, s.URL).PolicySource; got != "local" {
		t.Fatalf("tampered cache must be ignored, got source %q", got)
	}
	if code, _ := postChat(t, s.URL, "tok", "mock-eco", "包含 startup_banned 内容"); code != 400 {
		t.Fatalf("startup word list must still block, got %d", code)
	}
}

func TestOfflineFailClosedAndBackfill(t *testing.T) {
	gw := &fakeGateway{policyCustom: []string{"XH-200"}}
	gw.setFailChat(true)
	srv, s := startSidecarServer(t, gw, sidecar.Config{
		CustomWords: []string{"XH-200"}, HeartbeatIntervalSecs: 1,
	})
	srv.StartBackground()

	// A public request against a dead gateway is a retryable
	// outage; a confidential one (custom word) is denied outright.
	if code, body := postChat(t, s.URL, "tok", "mock-eco", "帮我写请假条"); code != 502 || !strings.Contains(body, "upstream_unavailable") {
		t.Fatalf("public offline: want 502 upstream_unavailable, got %d %s", code, body)
	}
	if code, body := postChat(t, s.URL, "tok", "mock-eco", "查一下 XH-200 的进度"); code != 503 || !strings.Contains(body, "sidecar_offline_blocked") {
		t.Fatalf("confidential offline: want 503 sidecar_offline_blocked, got %d %s", code, body)
	}
	st := sidecarStatus(t, s.URL)
	if st.Counters.OfflineUnreachable != 1 || st.Counters.OfflineBlocked != 1 {
		t.Fatalf("want 1 unreachable + 1 blocked, got %+v", st.Counters)
	}
	if st.OfflineBuffered != 2 {
		t.Fatalf("want 2 buffered offline decisions, got %d", st.OfflineBuffered)
	}
	if len(st.Recent) == 0 || st.Recent[0].Decision != "offline_blocked" {
		t.Fatalf("want latest decision offline_blocked, got %+v", st.Recent)
	}

	// The gateway recovers: the next heartbeat backfills the outage
	// and clears the buffer.
	gw.setFailChat(false)
	waitFor(t, "heartbeat backfill", func() bool { return gw.heartbeatCount() >= 1 })
	hb := gw.lastHeartbeat()
	var decoded struct {
		Version    string `json:"version"`
		ConfigHash string `json:"config_hash"`
		Guard      bool   `json:"guard_applied"`
		Offline    *struct {
			Events      int              `json:"events"`
			Blocked     int              `json:"blocked"`
			Unreachable int              `json:"unreachable"`
			ByCategory  map[string]int64 `json:"by_category"`
		} `json:"offline"`
	}
	if err := json.Unmarshal([]byte(hb), &decoded); err != nil {
		t.Fatalf("heartbeat must be valid JSON: %v", err)
	}
	if decoded.Version != sidecar.Version || len(decoded.ConfigHash) != 64 || !decoded.Guard {
		t.Fatalf("bad heartbeat body: %+v", decoded)
	}
	if decoded.Offline == nil || decoded.Offline.Events != 2 ||
		decoded.Offline.Blocked != 1 || decoded.Offline.Unreachable != 1 {
		t.Fatalf("want offline backfill 2/1/1, got %+v", decoded.Offline)
	}
	if decoded.Offline.ByCategory["custom:custom_word"] == 0 {
		t.Fatalf("want the custom-word category in the backfill, got %v", decoded.Offline.ByCategory)
	}
	waitFor(t, "offline buffer cleared", func() bool {
		return sidecarStatus(t, s.URL).OfflineBuffered == 0
	})
	// The heartbeat is authenticated with the token the sidecar saw
	// on the proxied requests; with no token it waits instead.
	if st := sidecarStatus(t, s.URL); st.LastHeartbeat == nil {
		t.Fatalf("want a recorded heartbeat time")
	}
}

func TestHeartbeatSendsTerminalState(t *testing.T) {
	gw := &fakeGateway{}
	srv, s := startSidecarServer(t, gw, sidecar.Config{
		AgentType: "dsh", Plugins: []string{"p1"},
		HeartbeatIntervalSecs: 1,
	})
	srv.StartBackground()
	if code, _ := postChat(t, s.URL, "tok", "mock-eco", "hello"); code != 200 {
		t.Fatalf("seed chat: got %d", code)
	}
	waitFor(t, "heartbeat", func() bool { return gw.heartbeatCount() >= 1 })
	var hb struct {
		AgentType  string   `json:"agent_type"`
		Version    string   `json:"version"`
		Plugins    []string `json:"plugins"`
		BinaryHash string   `json:"binary_hash"`
		OS         string   `json:"os"`
	}
	if err := json.Unmarshal([]byte(gw.lastHeartbeat()), &hb); err != nil {
		t.Fatal(err)
	}
	if hb.AgentType != "dsh" || hb.Version != sidecar.Version || len(hb.Plugins) != 1 || hb.Plugins[0] != "p1" {
		t.Fatalf("bad heartbeat body: %+v", hb)
	}
	if len(hb.BinaryHash) != 64 || hb.OS == "" {
		t.Fatalf("heartbeat must carry binary hash and OS, got %+v", hb)
	}
}
