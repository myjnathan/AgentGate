package cache_test

import (
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/cache"
)

func TestKeyDeterministic(t *testing.T) {
	body := []byte(`{"model":"mock-eco","messages":[{"role":"user","content":"hi"}]}`)
	k1 := cache.Key("ch_cloud", "public", body)
	k2 := cache.Key("ch_cloud", "public", body)
	if k1 != k2 {
		t.Fatalf("same input produced different keys: %s vs %s", k1, k2)
	}
	if len(k1) != 64 {
		t.Fatalf("key length = %d, want 64 hex chars (SHA-256)", len(k1))
	}
	if cache.Key("ch_other", "public", body) == k1 {
		t.Fatal("different channel must produce a different key")
	}
	if cache.Key("ch_cloud", "internal", body) == k1 {
		t.Fatal("different data level must produce a different key")
	}
	if cache.Key("ch_cloud", "public", []byte(`{"model":"mock-eco","messages":[{"role":"user","content":"hi!"}]}`)) == k1 {
		t.Fatal("different body must produce a different key")
	}
	if cache.Key("ch_cloud", "public", []byte(`{"model":"mock-eco","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)) == k1 {
		t.Fatal("different parameters must produce a different key (exact match)")
	}
}

func TestPutGetRoundtrip(t *testing.T) {
	c := cache.New(0, 0)
	body := []byte(`{"choices":[{"message":{"content":"hello"}}]}`)
	c.Put("k1", 200, body)
	status, got, ok := c.Get("k1")
	if !ok || status != 200 || string(got) != string(body) {
		t.Fatalf("Get = %d %q %v, want 200 %q true", status, got, ok, body)
	}
	// Mutating the returned body must not corrupt the stored entry.
	got[0] = 'X'
	_, got2, _ := c.Get("k1")
	if string(got2) != string(body) {
		t.Fatal("returned body shares storage with the cache entry")
	}
	if _, _, ok := c.Get("missing"); ok {
		t.Fatal("unknown key must miss")
	}
}

func TestExpiry(t *testing.T) {
	c := cache.New(50*time.Millisecond, 0)
	c.Put("k1", 200, []byte("v"))
	if _, _, ok := c.Get("k1"); !ok {
		t.Fatal("fresh entry must hit")
	}
	time.Sleep(60 * time.Millisecond)
	if _, _, ok := c.Get("k1"); ok {
		t.Fatal("expired entry must miss")
	}
	s := c.Stats()
	if s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("stats = hits %d misses %d, want 1/1", s.Hits, s.Misses)
	}
}

func TestDisabled(t *testing.T) {
	c := cache.New(0, 0)
	c.SetEnabled(false)
	c.Put("k1", 200, []byte("v"))
	if _, _, ok := c.Get("k1"); ok {
		t.Fatal("disabled cache must never hit")
	}
	s := c.Stats()
	if s.Enabled || s.Hits != 0 || s.Misses != 0 || s.Entries != 0 {
		t.Fatalf("disabled cache stats = %+v, want no counting while disabled", s)
	}
	c.SetEnabled(true)
	if _, _, ok := c.Get("k1"); ok {
		t.Fatal("entries stored while disabled must not appear after re-enabling")
	}
}

func TestMaxEntriesEviction(t *testing.T) {
	c := cache.New(0, 2)
	c.Put("k1", 200, []byte("1"))
	c.Put("k2", 200, []byte("2"))
	c.Put("k3", 200, []byte("3"))
	if _, _, ok := c.Get("k1"); ok {
		t.Fatal("oldest entry must be evicted at capacity (FIFO)")
	}
	if _, _, ok := c.Get("k2"); !ok {
		t.Fatal("k2 must survive")
	}
	if _, _, ok := c.Get("k3"); !ok {
		t.Fatal("k3 must survive")
	}
	// Re-storing an existing key refreshes it without evicting others.
	c.Put("k2", 200, []byte("2b"))
	if _, _, ok := c.Get("k3"); !ok {
		t.Fatal("refreshing k2 must not evict k3")
	}
	if s := c.Stats(); s.Entries != 2 {
		t.Fatalf("entries = %d, want 2", s.Entries)
	}
}

func TestOversizeRejected(t *testing.T) {
	c := cache.New(0, 0)
	big := []byte(strings.Repeat("x", cache.MaxBodyBytes+1))
	c.Put("big", 200, big)
	if _, _, ok := c.Get("big"); ok {
		t.Fatal("oversized body must not be stored")
	}
	c.Put("err", 500, []byte("boom"))
	if _, _, ok := c.Get("err"); ok {
		t.Fatal("non-200 response must not be stored")
	}
}

func TestStatsAndClear(t *testing.T) {
	c := cache.New(time.Hour, 0)
	c.Put("k1", 200, []byte("1"))
	c.Put("k2", 200, []byte("2"))
	if _, _, ok := c.Get("k1"); !ok {
		t.Fatal("k1 must hit")
	}
	if _, _, ok := c.Get("missing"); ok {
		t.Fatal("missing must miss")
	}
	s := c.Stats()
	if !s.Enabled || s.TTLSeconds != 3600 || s.MaxEntries != 1000 || s.Entries != 2 || s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("stats = %+v, want enabled/3600/1000/2/1/1", s)
	}
	if n := c.Clear(); n != 2 {
		t.Fatalf("Clear = %d, want 2", n)
	}
	s = c.Stats()
	if s.Entries != 0 {
		t.Fatalf("entries after clear = %d, want 0", s.Entries)
	}
	if s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("clear must not reset counters, stats = %+v", s)
	}
}
