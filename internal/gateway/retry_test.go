package gateway

import (
	"testing"
	"time"
)

// Pure-function tests for the F33 retry layer: classification,
// budget defaults and backoff bounds. HTTP behaviour is covered by
// gateway_retry_test.go (package gateway_test).

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		code int
		want failureClass
	}{
		{200, classSuccess},
		{201, classSuccess},
		{301, classSuccess},
		{400, classClient},
		{404, classClient},
		{422, classClient},
		{401, classAuth},
		{403, classAuth},
		{429, classTransient},
		{500, classTransient},
		{502, classTransient},
		{503, classTransient},
	}
	for _, c := range cases {
		if got := classifyStatus(c.code); got != c.want {
			t.Errorf("classifyStatus(%d) = %q, want %q", c.code, got, c.want)
		}
	}
}

func TestMaxTransientRetriesDefault(t *testing.T) {
	g := &Gateway{policy: Policy{}}
	if got := g.maxTransientRetries(); got != 2 {
		t.Fatalf("default maxTransientRetries = %d, want 2", got)
	}
	g.policy.MaxTransientRetries = 1
	if got := g.maxTransientRetries(); got != 1 {
		t.Fatalf("configured maxTransientRetries = %d, want 1", got)
	}
}

func TestRetryBackoffBounds(t *testing.T) {
	g := &Gateway{policy: Policy{RetryBackoff: 10 * time.Millisecond}}
	// Attempt 0: base .. 1.5x base (jitter adds up to half the wait).
	if d := g.retryBackoff(0); d < 10*time.Millisecond || d > 15*time.Millisecond {
		t.Fatalf("retryBackoff(0) = %v, want [10ms, 15ms]", d)
	}
	if d := g.retryBackoff(1); d < 20*time.Millisecond || d > 30*time.Millisecond {
		t.Fatalf("retryBackoff(1) = %v, want [20ms, 30ms]", d)
	}
	// Far out: the wait caps at 2s (plus jitter), it never grows
	// without bound.
	if d := g.retryBackoff(20); d < 2*time.Second || d > 3*time.Second {
		t.Fatalf("retryBackoff(20) = %v, want [2s, 3s]", d)
	}
	// Zero policy backoff falls back to the 100ms default base.
	g.policy.RetryBackoff = 0
	if d := g.retryBackoff(0); d < 100*time.Millisecond || d > 150*time.Millisecond {
		t.Fatalf("default retryBackoff(0) = %v, want [100ms, 150ms]", d)
	}
}
