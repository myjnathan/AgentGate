package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/gateway"
	"github.com/myjnathan/agentgate/internal/mockllm"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

// testEnv spins up a gateway backed by the demo seed and a mock upstream.
type testEnv struct {
	srv     *httptest.Server
	st      *store.Store
	al      *audit.Log
	byoaTok string
	mgdTok  string
}

func setup(t *testing.T) *testEnv {
	return setupWithPolicy(t, gateway.Policy{
		BannedWords:   seed.DemoBannedWords,
		CustomWords:   seed.DemoCustomWords,
		RedactForTier: map[store.Tier]bool{store.TierManaged: true},
	})
}

func setupWithPolicy(t *testing.T, pol gateway.Policy) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secretbox.New("test-master-key-123456")
	au := auth.New(st)

	mock := httptest.NewServer(mockllm.Handler())
	t.Cleanup(mock.Close)
	// Seed expects base URL of the mock; use the mock server URL directly.
	// seed.Demo writes channels pointing at selfBaseURL + /mock/v1, so
	// pass the mock host without the /mock suffix handling: instead we
	// seed with the mock URL trimmed of nothing — Demo builds
	// selfBaseURL + "/mock/v1"; mock serves at /mock/v1 already.
	// So selfBaseURL should be the mock server root.
	if err := seed.Demo(st, box, mock.URL); err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(st, au, box, al, pol)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", gw.Models)
	mux.HandleFunc("POST /v1/chat/completions", gw.ChatCompletions)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mkToken := func(username string, tier store.Tier) string {
		var user *store.User
		st.Read(func(s *store.State) {
			for _, u := range s.Users {
				if u.Username == username {
					cp := *u
					user = &cp
				}
			}
		})
		dc := au.StartDeviceCode("dsh", tier)
		if _, err := au.ApproveDeviceCode(dc.Code, user, "test device"); err != nil {
			t.Fatal(err)
		}
		_, plain := au.PollDeviceCode(dc.Code, dc.Secret)
		if plain == "" {
			t.Fatal("no token issued")
		}
		return plain
	}
	return &testEnv{srv: srv, st: st, al: al,
		byoaTok: mkToken("lixiaofeng", store.TierBYOA),
		mgdTok:  mkToken("chenjing", store.TierManaged)}
}

func (e *testEnv) chat(t *testing.T, token, model, content string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": content}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func TestCleanPublicCallAllowed(t *testing.T) {
	e := setup(t)
	code, body := e.chat(t, e.byoaTok, "mock-eco", "你好，帮我写一封请假邮件")
	if code != 200 || !strings.Contains(body, "mock-model") {
		t.Fatalf("expected 200 mock reply, got %d %s", code, body)
	}
}

func TestBannedWordBlocked(t *testing.T) {
	e := setup(t)
	code, body := e.chat(t, e.mgdTok, "mock-eco", "this contains test_sensitive word")
	if code != 400 || !strings.Contains(body, "request_rejected") {
		t.Fatalf("expected block, got %d %s", code, body)
	}
}

func TestBYOAConfidentialBlocked(t *testing.T) {
	e := setup(t)
	// Custom word XH-200 classifies as confidential; BYOA tier must be blocked.
	code, body := e.chat(t, e.byoaTok, "mock-pro", "请分析 XH-200 项目的进展")
	if code != 403 || !strings.Contains(body, "data_level_forbidden") {
		t.Fatalf("expected 403 for BYOA confidential, got %d %s", code, body)
	}
}

func TestManagedConfidentialRoutedToPrivate(t *testing.T) {
	e := setup(t)
	// mock-pro is served by both channels; confidential content must be
	// routed to the private channel (max level secret), and succeed.
	code, body := e.chat(t, e.mgdTok, "mock-pro", "请分析 XH-200 项目的进展")
	if code != 200 {
		t.Fatalf("expected 200 for managed confidential via private channel, got %d %s", code, body)
	}
}

func TestManagedRedactionOfPII(t *testing.T) {
	e := setup(t)
	// Managed tier redacts PII before forwarding; the mock echoes the
	// (redacted) prompt, and the gateway restores placeholders in the
	// response, so the client sees the original value back.
	code, body := e.chat(t, e.mgdTok, "mock-eco", "我的手机号是13812345678，帮我记下来")
	if code != 200 {
		t.Fatalf("expected 200, got %d %s", code, body)
	}
	if !strings.Contains(body, "13812345678") {
		t.Fatalf("expected restored phone in response, got %s", body)
	}
}

func TestSecretLevelBlockedWhenNoChannel(t *testing.T) {
	e := setup(t)
	// A banned word is blocked earlier; use header-declared secret level
	// with mock-eco, which no secret-capable channel serves.
	body, _ := json.Marshal(map[string]any{
		"model":    "mock-eco",
		"messages": []map[string]string{{"role": "user", "content": "普通内容"}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.mgdTok)
	req.Header.Set("X-AgentGate-Data-Level", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("expected 403 no_channel_for_data_level, got %d", resp.StatusCode)
	}
}
