// Dispatcher delivers alerts to configured webhooks asynchronously,
// so a slow or dead webhook can never block the request path.
//
// Delivery: POST with a 10s timeout; on any failure (network error or
// non-2xx) retry up to 3 times with backoff, then give up and report
// the failure. Reported errors are coarse classes ("http_500",
// "network_error", "timeout") — the raw error may embed the webhook
// URL, which can carry a bot key, so it must never reach the audit log
// or the admin API.
package alert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// DeliveryStatus is the last known delivery outcome for one webhook
// (runtime state, like channel health; not persisted).
type DeliveryStatus struct {
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	LastOK        bool      `json:"last_ok"`
	LastError     string    `json:"last_error,omitempty"`
	Attempts      int       `json:"attempts"`
}

// Result is reported after a delivery finishes (success or final
// failure). Attempts counts all tries including the first.
type Result struct {
	Webhook  store.Webhook
	Alert    Alert
	Attempts int
	Status   int   // last HTTP status (0 = no response received)
	Err      error // nil on success; ErrClass(err) for the audit-safe text
}

// ErrClass maps a delivery error to an audit-safe class string.
// The raw error is never surfaced: it may embed the webhook URL.
func ErrClass(err error, status int) string {
	if err == nil && status >= 200 && status < 300 {
		return ""
	}
	var ce *configError
	if errors.As(err, &ce) {
		return "config_error"
	}
	if status != 0 {
		return fmt.Sprintf("http_%d", status)
	}
	if err == nil {
		return "unknown_error"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var uerr interface{ Timeout() bool }
	if errors.As(err, &uerr) && uerr.Timeout() {
		return "timeout"
	}
	return "network_error"
}

// configError marks build/decrypt failures (bad config, not the network).
type configError struct{ err error }

func (e *configError) Error() string { return e.err.Error() }
func (e *configError) Unwrap() error { return e.err }

type Dispatcher struct {
	st      *store.Store
	box     *secretbox.Box
	client  *http.Client
	backoff []time.Duration
	queue   chan Alert
	stop    chan struct{}
	wg      sync.WaitGroup

	mu       sync.Mutex
	status   map[string]DeliveryStatus
	dropped  int
	onResult func(Result)
}

// Option configures a Dispatcher.
type Option func(*Dispatcher)

// WithClient overrides the HTTP client (tests).
func WithClient(c *http.Client) Option {
	return func(d *Dispatcher) { d.client = c }
}

// WithBackoff overrides the retry delays; len(backoff) is the number
// of retries after the first attempt (default 3: 1s, 2s, 4s).
func WithBackoff(b []time.Duration) Option {
	return func(d *Dispatcher) { d.backoff = b }
}

// WithQueueSize overrides the alert queue depth (default 64).
func WithQueueSize(n int) Option {
	return func(d *Dispatcher) { d.queue = make(chan Alert, n) }
}

// WithOnResult is called after every finished delivery.
func WithOnResult(fn func(Result)) Option {
	return func(d *Dispatcher) { d.onResult = fn }
}

func New(st *store.Store, box *secretbox.Box, opts ...Option) *Dispatcher {
	d := &Dispatcher{st: st, box: box,
		client:  &http.Client{Timeout: 10 * time.Second},
		backoff: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		queue:   make(chan Alert, 64),
		stop:    make(chan struct{}),
		status:  map[string]DeliveryStatus{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Start launches the delivery worker.
func (d *Dispatcher) Start() {
	d.wg.Add(1)
	go d.loop()
}

// Stop drains nothing: queued alerts are best-effort and dropped on
// shutdown, like F1 channel health which also resets on restart.
func (d *Dispatcher) Stop() {
	close(d.stop)
	d.wg.Wait()
}

func (d *Dispatcher) loop() {
	defer d.wg.Done()
	for {
		select {
		case a := <-d.queue:
			d.deliverAll(a)
		case <-d.stop:
			return
		}
	}
}

// Notify enqueues an alertable audit event for delivery. It never
// blocks the caller: when the queue is full the alert is dropped and
// counted.
func (d *Dispatcher) Notify(ev audit.Event) {
	a, ok := FromEvent(ev)
	if !ok {
		return
	}
	select {
	case d.queue <- a:
	default:
		d.mu.Lock()
		d.dropped++
		d.mu.Unlock()
	}
}

// Dropped returns how many alerts were dropped because the queue was full.
func (d *Dispatcher) Dropped() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

// Status snapshots the last delivery outcome per webhook.
func (d *Dispatcher) Status() map[string]DeliveryStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]DeliveryStatus, len(d.status))
	for k, v := range d.status {
		out[k] = v
	}
	return out
}

// deliverAll sends one alert to every enabled webhook.
func (d *Dispatcher) deliverAll(a Alert) {
	var webhooks []*store.Webhook
	d.st.Read(func(st *store.State) {
		for _, wh := range st.Webhooks {
			if wh.Enabled {
				cp := *wh
				webhooks = append(webhooks, &cp)
			}
		}
	})
	for _, wh := range webhooks {
		attempts, err, status := d.Send(*wh, a)
		d.record(*wh, a, attempts, err, status)
	}
}

// Send delivers one alert to one webhook with retries and returns
// (attempts, err, lastHTTPStatus). Attempts is 1 + number of retries.
func (d *Dispatcher) Send(wh store.Webhook, a Alert) (int, error, int) {
	body, err := BuildPayload(wh.Kind, a)
	if err != nil {
		return 1, &configError{err}, 0
	}
	secret := ""
	if wh.SecretEnc != "" && d.box != nil {
		secret, err = d.box.Decrypt(wh.SecretEnc)
		if err != nil {
			return 1, &configError{fmt.Errorf("decrypt webhook secret: %w", err)}, 0
		}
	}
	target := wh.URL
	if secret != "" && wh.Kind != KindGeneric {
		target, err = SignedURL(wh.URL, secret, time.Now())
		if err != nil {
			return 1, err, 0
		}
	}
	var lastErr error
	lastStatus := 0
	for attempt := 0; attempt <= len(d.backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(d.backoff[attempt-1]):
			case <-d.stop:
				return attempt, lastErr, lastStatus
			}
		}
		status, err := d.post(target, wh.Kind, secret, body)
		lastStatus = status
		if err == nil && status >= 200 && status < 300 {
			return attempt + 1, nil, status
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("webhook returned status %d", status)
		}
	}
	return len(d.backoff) + 1, lastErr, lastStatus
}

func (d *Dispatcher) post(target, kind, secret string, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" && kind == KindGeneric {
		req.Header.Set("X-AgentGate-Signature", HeaderSignature(secret, body))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func (d *Dispatcher) record(wh store.Webhook, a Alert, attempts int, err error, status int) {
	d.mu.Lock()
	d.status[wh.ID] = DeliveryStatus{LastAttemptAt: time.Now(), LastOK: err == nil,
		LastError: ErrClass(err, status), Attempts: attempts}
	d.mu.Unlock()
	if d.onResult != nil {
		d.onResult(Result{Webhook: wh, Alert: a, Attempts: attempts, Status: status, Err: err})
	}
}
