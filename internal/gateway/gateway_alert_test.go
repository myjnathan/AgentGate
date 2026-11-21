package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// TestBlockedCallTriggersWebhookAlert is the F5 acceptance test: a
// banned-word block is audited, the audit hook notifies the
// dispatcher, and the configured generic webhook receives a
// structured alert containing no sensitive text.
func TestBlockedCallTriggersWebhookAlert(t *testing.T) {
	e := setup(t)

	var mu sync.Mutex
	var bodies []string
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer recv.Close()

	box, _ := secretbox.New("test-master-key-123456")
	d := alert.New(e.st, box,
		alert.WithBackoff([]time.Duration{}))
	d.Start()
	defer d.Stop()
	e.al.SetNotifyHook(d.Notify)
	e.st.WithLock(func(s *store.State) {
		s.Webhooks["wh_smoke"] = &store.Webhook{ID: "wh_smoke", Name: "smoke",
			URL: recv.URL, Kind: alert.KindGeneric, Enabled: true}
	})

	code, body := e.chat(t, e.byoaTok, "mock-eco", "包含 test_sensitive 内容")
	if code != 400 || !strings.Contains(body, "request_rejected") {
		t.Fatalf("expected banned-word block, got %d %s", code, body)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("webhook received %d alerts, want 1", len(bodies))
	}
	if strings.Contains(bodies[0], "test_sensitive") {
		t.Fatalf("alert must not contain the matched sensitive text: %s", bodies[0])
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &v); err != nil {
		t.Fatal(err)
	}
	if v["source"] != "agentgate" || v["severity"] != "critical" ||
		v["action"] != "model_call_blocked" || v["actor"] != "lixiaofeng" {
		t.Fatalf("unexpected alert payload: %s", bodies[0])
	}
}
