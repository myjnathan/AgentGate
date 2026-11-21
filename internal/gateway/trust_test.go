package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/myjnathan/agentgate/internal/gateway"
	"github.com/myjnathan/agentgate/internal/store"
)

// recordingUpstream serves /<name>/v1/chat/completions, records the exact
// request body each upstream received (so tests can prove whether the
// gateway desensitized before forwarding), and echoes the last user
// message so placeholder restore stays observable.
type recordingUpstream struct {
	mu     sync.Mutex
	hits   map[string]int
	bodies map[string][]byte
}

func (u *recordingUpstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		name := parts[0]
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hits[name]++
		u.bodies[name] = body
		u.mu.Unlock()
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		last := ""
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				_ = json.Unmarshal(req.Messages[i].Content, &last)
				break
			}
		}
		reply := "[upstream-" + name + "] " + last
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 0, "model": "mock-pro",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{
				"role": "assistant", "content": reply}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})
	return mux
}

func (u *recordingUpstream) hitCount(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[name]
}

func (u *recordingUpstream) body(name string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return string(u.bodies[name])
}

// setupTrust re-points the four seeded demo channels at a recording
// upstream: cloud -> /cloud, desensitized cloud -> /desens,
// dedicated/VPC -> /vpc, local model -> /local.
func setupTrust(t *testing.T, pol gateway.Policy) (*testEnv, *recordingUpstream) {
	t.Helper()
	e := setupWithPolicy(t, pol)
	up := &recordingUpstream{hits: map[string]int{}, bodies: map[string][]byte{}}
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)
	e.st.WithLock(func(s *store.State) {
		points := map[string]string{
			"ch_cloud":      "/cloud/v1",
			"ch_cloud_conf": "/desens/v1",
			"ch_private":    "/vpc/v1",
			"ch_local":      "/local/v1",
		}
		for id, suffix := range points {
			if ch, ok := s.Channels[id]; ok {
				ch.BaseURL = srv.URL + suffix
			}
		}
	})
	return e, up
}

func trustPolicy() gateway.Policy {
	return gateway.Policy{
		BannedWords:   []string{"test_sensitive"},
		CustomWords:   []string{"XH-200", "星河 XH-200"},
		RedactForTier: map[store.Tier]bool{store.TierManaged: true},
	}
}

// chatWithLevel is e.chat plus an optional caller-declared data level
// header (the endpoint-sidecar signal for content from encrypted files).
func chatWithLevel(e *testEnv, token, model, content, level string) (int, string) {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": content}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if level != "" {
		req.Header.Set("X-AgentGate-Data-Level", level)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func TestTrustGradientRoutesByLevel(t *testing.T) {
	e, up := setupTrust(t, trustPolicy())

	// Public/internal data: signed cloud, original text.
	code, _ := e.chat(t, e.mgdTok, "mock-eco", "帮我写请假条")
	if code != 200 || up.hitCount("cloud") != 1 {
		t.Fatalf("public call should reach signed cloud once, got %d (cloud hits %d)", code, up.hitCount("cloud"))
	}

	// Confidential data (XH-200): default route is the signed cloud, but
	// the gateway must desensitize first — the upstream sees the
	// placeholder, never the original code name.
	code, body := e.chat(t, e.mgdTok, "mock-pro", "请分析 XH-200 项目的进展")
	if code != 200 {
		t.Fatalf("confidential call: got %d %s, want 200", code, body)
	}
	if got := up.hitCount("desens"); got != 1 {
		t.Fatalf("confidential call should hit desensitized cloud once, got %d", got)
	}
	if up.hitCount("vpc") != 0 || up.hitCount("local") != 0 {
		t.Fatalf("confidential call must not reach vpc/local (vpc %d, local %d)", up.hitCount("vpc"), up.hitCount("local"))
	}
	// The recorded body is JSON, so "<CUSTOM_1>" arrives \u-escaped;
	// assert on the placeholder name, which survives escaping.
	if sent := up.body("desens"); strings.Contains(sent, "XH-200") || !strings.Contains(sent, "CUSTOM_1") {
		t.Fatalf("signed cloud must receive redacted text, got %s", sent)
	}
	// The response restores the placeholder for the client.
	if !strings.Contains(body, "XH-200") {
		t.Fatalf("expected restored XH-200 in response, got %s", body)
	}
	// Audit records the desensitized hop (decision stays "reroute", the
	// existing convention for confidential calls; the desensitization
	// itself is recorded in the detail).
	found := false
	for _, ev := range e.al.All() {
		if ev.Kind == "model_call" && ev.Channel == "cloud-contract-desensitized (mock)" && ev.Detail["desensitized"] == "signed_cloud" {
			found = true
		}
	}
	if !found {
		t.Fatal("audit should record the confidential call on the desensitized channel with desensitized=signed_cloud")
	}

	// Top-secret data (header-declared): only the local model.
	code, _ = chatWithLevel(e, e.mgdTok, "mock-pro", "普通内容", "secret")
	if code != 200 || up.hitCount("local") != 1 {
		t.Fatalf("secret call should reach local model once, got %d (local hits %d)", code, up.hitCount("local"))
	}
	if up.hitCount("desens") != 1 || up.hitCount("vpc") != 0 {
		t.Fatalf("secret call must not reach any cloud (desens %d, vpc %d)", up.hitCount("desens"), up.hitCount("vpc"))
	}
}

func TestConfidentialDesensitizedEvenWhenTierRedactionOff(t *testing.T) {
	// The F17 guarantee is structural: the trust type forces
	// desensitization, so switching tier redaction off must not leak
	// confidential text to a signed cloud.
	pol := trustPolicy()
	pol.RedactForTier = nil
	e, up := setupTrust(t, pol)
	code, _ := e.chat(t, e.mgdTok, "mock-pro", "请分析 XH-200 项目的进展")
	if code != 200 {
		t.Fatalf("confidential call: got %d, want 200", code)
	}
	if sent := up.body("desens"); strings.Contains(sent, "XH-200") {
		t.Fatalf("signed cloud received original confidential text with tier redaction off: %s", sent)
	}
}

func TestConfidentialToDedicatedCloudRaw(t *testing.T) {
	// With no signed-cloud channel for confidential data, the request
	// falls to the dedicated-cloud/VPC channel and arrives as-is
	// (tier redaction off here, so nothing else redacts it).
	pol := trustPolicy()
	pol.RedactForTier = nil
	e, up := setupTrust(t, pol)
	e.st.WithLock(func(s *store.State) {
		s.Channels["ch_cloud_conf"].Disabled = true
		s.Channels["ch_local"].Disabled = true
	})
	code, body := e.chat(t, e.mgdTok, "mock-pro", "请分析 XH-200 项目的进展")
	if code != 200 {
		t.Fatalf("confidential call to dedicated cloud: got %d %s, want 200", code, body)
	}
	if got := up.hitCount("vpc"); got != 1 {
		t.Fatalf("expected dedicated cloud hit once, got %d", got)
	}
	if sent := up.body("vpc"); !strings.Contains(sent, "XH-200") {
		t.Fatalf("dedicated cloud should receive original text, got %s", sent)
	}
}

func TestSecretBlockedWithoutLocalChannel(t *testing.T) {
	e, up := setupTrust(t, trustPolicy())
	e.st.WithLock(func(s *store.State) {
		s.Channels["ch_local"].Disabled = true
	})
	code, body := chatWithLevel(e, e.mgdTok, "mock-pro", "普通内容", "secret")
	if code != 403 || !strings.Contains(body, "no_compliant_channel") {
		t.Fatalf("secret call without local model: got %d %s, want 403 no_compliant_channel", code, body)
	}
	if !strings.Contains(body, "local model") {
		t.Fatalf("block message should say what is missing, got %s", body)
	}
	if up.hitCount("vpc") != 0 || up.hitCount("desens") != 0 {
		t.Fatalf("blocked secret call must not reach any upstream (vpc %d, desens %d)", up.hitCount("vpc"), up.hitCount("desens"))
	}
	found := false
	for _, ev := range e.al.All() {
		if ev.Kind == "violation" && ev.Decision == "block" && ev.Detail["reason"] == "no_compliant_channel" {
			found = true
		}
	}
	if !found {
		t.Fatal("audit should record the block with reason no_compliant_channel")
	}
	// Confidential data still flows in the same environment (via the
	// desensitized cloud) — the block is scoped to top-secret only.
	code, _ = e.chat(t, e.mgdTok, "mock-pro", "请分析 XH-200 项目的进展")
	if code != 200 {
		t.Fatalf("confidential call should still succeed, got %d", code)
	}
}

func TestSecretBlockedWhenOnlyDedicatedCloud(t *testing.T) {
	// A dedicated-cloud channel that may receive secret data by level
	// is still not compliant for secret data by trust — the error must
	// be no_compliant_channel, not the generic no_channel_for_data_level.
	e, _ := setupTrust(t, trustPolicy())
	e.st.WithLock(func(s *store.State) {
		s.Channels["ch_cloud_conf"].Disabled = true
		s.Channels["ch_local"].Disabled = true
	})
	code, body := chatWithLevel(e, e.mgdTok, "mock-pro", "普通内容", "secret")
	if code != 403 || !strings.Contains(body, "no_compliant_channel") {
		t.Fatalf("secret call with only dedicated cloud: got %d %s, want 403 no_compliant_channel", code, body)
	}
}
