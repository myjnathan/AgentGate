package alert_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func testAlert() alert.Alert {
	return alert.Alert{Severity: "critical", Title: "请求被防护策略拦截",
		Kind: "violation", Action: "model_call_blocked", Actor: "lixiaofeng",
		Detail: map[string]string{"findings": "banned:banned_word"},
		Time:   time.Now().UTC()}
}

func TestSendSuccessFirstTry(t *testing.T) {
	st := testStore(t)
	box, _ := secretbox.New("test-master-key-123456")
	var got struct {
		mu     sync.Mutex
		bodies []string
		sig    string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.bodies = append(got.bodies, string(b))
		got.sig = r.Header.Get("X-AgentGate-Signature")
		got.mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	enc, _ := box.Encrypt("webhook-secret")
	wh := store.Webhook{ID: "wh_1", Name: "test", URL: srv.URL, Kind: alert.KindGeneric, SecretEnc: enc, Enabled: true}
	d := alert.New(st, box, alert.WithBackoff([]time.Duration{}))
	attempts, err, status := d.Send(wh, testAlert())
	if err != nil || status != 200 || attempts != 1 {
		t.Fatalf("Send = attempts %d status %d err %v", attempts, status, err)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if len(got.bodies) != 1 {
		t.Fatalf("got %d requests, want 1", len(got.bodies))
	}
	// The signature header must verify against the received body.
	mac := hmac.New(sha256.New, []byte("webhook-secret"))
	mac.Write([]byte(got.bodies[0]))
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); got.sig != want {
		t.Fatalf("signature header %q, want %q", got.sig, want)
	}
	var v map[string]any
	_ = json.Unmarshal([]byte(got.bodies[0]), &v)
	if v["title"] != "请求被防护策略拦截" || v["source"] != "agentgate" {
		t.Fatalf("unexpected body: %s", got.bodies[0])
	}
}

func TestSendRetriesThenSucceeds(t *testing.T) {
	st := testStore(t)
	box, _ := secretbox.New("test-master-key-123456")
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	wh := store.Webhook{ID: "wh_1", URL: srv.URL, Kind: alert.KindGeneric, Enabled: true}
	d := alert.New(st, box, alert.WithBackoff([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}))
	attempts, err, status := d.Send(wh, testAlert())
	if err != nil || status != 200 || attempts != 3 {
		t.Fatalf("Send = attempts %d status %d err %v, want 3/200/nil", attempts, status, err)
	}
}

func TestSendGivesUpAfterThreeRetries(t *testing.T) {
	st := testStore(t)
	box, _ := secretbox.New("test-master-key-123456")
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(503)
	}))
	defer srv.Close()

	wh := store.Webhook{ID: "wh_1", URL: srv.URL, Kind: alert.KindGeneric, Enabled: true}
	d := alert.New(st, box, alert.WithBackoff([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}))
	attempts, err, status := d.Send(wh, testAlert())
	if err == nil || attempts != 4 || status != 503 {
		t.Fatalf("Send = attempts %d status %d err %v, want 4/503/non-nil", attempts, status, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 4 {
		t.Fatalf("server saw %d calls, want 4 (1 + 3 retries)", calls)
	}
	if got := alert.ErrClass(err, status); got != "http_503" {
		t.Fatalf("ErrClass = %q, want http_503", got)
	}
}

func TestSendNetworkFailureClass(t *testing.T) {
	st := testStore(t)
	box, _ := secretbox.New("test-master-key-123456")
	// Unroutable loopback port: connection refused, no HTTP status.
	wh := store.Webhook{ID: "wh_1", URL: "http://127.0.0.1:1/hook", Kind: alert.KindGeneric, Enabled: true}
	d := alert.New(st, box, alert.WithBackoff([]time.Duration{}))
	_, err, status := d.Send(wh, testAlert())
	if err == nil || status != 0 {
		t.Fatalf("Send err=%v status=%d, want error with status 0", err, status)
	}
	if got := alert.ErrClass(err, status); got != "network_error" {
		t.Fatalf("ErrClass = %q, want network_error", got)
	}
}

func TestNotifyDeliversAsyncAndSkipsDisabled(t *testing.T) {
	st := testStore(t)
	box, _ := secretbox.New("test-master-key-123456")
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	st.WithLock(func(s *store.State) {
		s.Webhooks["wh_on"] = &store.Webhook{ID: "wh_on", Name: "on", URL: srv.URL, Kind: alert.KindGeneric, Enabled: true}
		s.Webhooks["wh_off"] = &store.Webhook{ID: "wh_off", Name: "off", URL: srv.URL, Kind: alert.KindGeneric, Enabled: false}
	})
	var results []alert.Result
	var rmu sync.Mutex
	d := alert.New(st, box,
		alert.WithBackoff([]time.Duration{}),
		alert.WithOnResult(func(r alert.Result) {
			rmu.Lock()
			results = append(results, r)
			rmu.Unlock()
		}))
	d.Start()
	defer d.Stop()

	// A non-alertable event produces no delivery.
	d.Notify(audit.Event{Kind: "model_call", Action: "chat_completions", Decision: "allow"})
	// An alertable event delivers exactly once (disabled webhook skipped).
	d.Notify(audit.Event{Kind: "violation", Action: "model_call_blocked", Decision: "block", Actor: "lixiaofeng"})

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
		t.Fatalf("got %d deliveries, want 1", len(bodies))
	}
	if strings.Contains(bodies[0], "test_sensitive") {
		t.Fatalf("delivery must not contain sensitive text: %s", bodies[0])
	}
	rmu.Lock()
	defer rmu.Unlock()
	if len(results) != 1 || results[0].Err != nil || results[0].Attempts != 1 {
		t.Fatalf("unexpected results: %+v", results)
	}
	st2 := d.Status()["wh_on"]
	if !st2.LastOK || st2.Attempts != 1 {
		t.Fatalf("unexpected status: %+v", st2)
	}
}

func TestSignedPlatformDelivery(t *testing.T) {
	st := testStore(t)
	box, _ := secretbox.New("test-master-key-123456")
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	enc, _ := box.Encrypt("SECtestsecret")
	wh := store.Webhook{ID: "wh_1", URL: srv.URL + "/robot/send?access_token=tok123", Kind: alert.KindDingTalk, SecretEnc: enc, Enabled: true}
	d := alert.New(st, box, alert.WithBackoff([]time.Duration{}))
	if _, err, _ := d.Send(wh, testAlert()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotURL, "access_token=tok123") || !strings.Contains(gotURL, "timestamp=") || !strings.Contains(gotURL, "sign=") {
		t.Fatalf("signed request URL missing parameters: %s", gotURL)
	}
}
