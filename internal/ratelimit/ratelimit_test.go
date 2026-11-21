package ratelimit_test

import (
	"sync"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/ratelimit"
)

func TestRPMBoundary(t *testing.T) {
	l := ratelimit.New()
	now := time.Now()
	for i := 0; i < 5; i++ {
		ok, _, _ := l.Allow("tok1", 5, 0, now)
		if !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	ok, kind, retry := l.Allow("tok1", 5, 0, now)
	if ok || kind != "rpm" || retry <= 0 {
		t.Fatalf("6th request: ok=%v kind=%q retry=%v, want denied/rpm/>0", ok, kind, retry)
	}
	// A different token has its own budget.
	ok, _, _ = l.Allow("tok2", 5, 0, now)
	if !ok {
		t.Fatal("other token should not share the budget")
	}
}

func TestSlidingWindowExpiry(t *testing.T) {
	l := ratelimit.New()
	now := time.Now()
	for i := 0; i < 2; i++ {
		l.Allow("tok1", 2, 0, now)
	}
	if ok, _, _ := l.Allow("tok1", 2, 0, now); ok {
		t.Fatal("should be denied inside the window")
	}
	// 61s later the window has slid past both requests.
	if ok, _, _ := l.Allow("tok1", 2, 0, now.Add(61*time.Second)); !ok {
		t.Fatal("should be allowed after the window slides")
	}
	// Only the second request is still in the window at +30s.
	if ok, _, _ := l.Allow("tok1", 2, 0, now.Add(30*time.Second)); !ok {
		t.Fatal("one slot should have freed at +30s")
	}
}

func TestTPMRecordedAfterResponse(t *testing.T) {
	l := ratelimit.New()
	now := time.Now()
	// TPM=10. The first request is allowed (nothing recorded yet).
	if ok, _, _ := l.Allow("tok1", 0, 10, now); !ok {
		t.Fatal("first request should be allowed")
	}
	// The response reports 10 tokens; only now does the budget fill.
	l.RecordTokens("tok1", 10, now)
	if ok, kind, _ := l.Allow("tok1", 0, 10, now); ok || kind != "tpm" {
		t.Fatalf("should be denied by tpm, got ok=%v kind=%q", ok, kind)
	}
	// After the window slides, tokens are forgotten.
	if ok, _, _ := l.Allow("tok1", 0, 10, now.Add(61*time.Second)); !ok {
		t.Fatal("should be allowed after the window slides")
	}
}

func TestZeroMeansUnlimited(t *testing.T) {
	l := ratelimit.New()
	now := time.Now()
	for i := 0; i < 100; i++ {
		if ok, _, _ := l.Allow("tok1", 0, 0, now); !ok {
			t.Fatalf("request %d should be allowed with no limits", i+1)
		}
	}
	l.RecordTokens("tok1", 1_000_000, now)
	if ok, _, _ := l.Allow("tok1", 0, 0, now); !ok {
		t.Fatal("unlimited TPM should ignore recorded usage")
	}
}

func TestRetryAfterShrinksAsWindowSlides(t *testing.T) {
	l := ratelimit.New()
	now := time.Now()
	l.Allow("tok1", 1, 0, now)
	_, _, retry := l.Allow("tok1", 1, 0, now.Add(10*time.Second))
	if retry != 50*time.Second {
		t.Fatalf("retryAfter = %v, want 50s", retry)
	}
}

func TestConcurrentAllow(t *testing.T) {
	l := ratelimit.New()
	now := time.Now()
	var wg sync.WaitGroup
	allowed := make(chan bool, 200)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, _ := l.Allow("tok1", 100, 0, now)
			allowed <- ok
		}()
	}
	wg.Wait()
	close(allowed)
	n := 0
	for ok := range allowed {
		if ok {
			n++
		}
	}
	if n != 100 {
		t.Fatalf("allowed = %d, want exactly 100 under concurrency", n)
	}
}
