// Package cache is the exact-match response cache for the model
// gateway (backlog F11). A cached response is served only when the
// incoming request is byte-identical to a previous one (same serving
// channel, same full request body) and still within its TTL.
//
// The cache is deliberately narrow: it is a cost/latency optimisation
// for repeatable, non-dynamic calls (classification, fixed extraction,
// FAQ-style prompts), not a general memoisation layer. Anything that
// could leak across a trust boundary is never stored — the gateway
// only hands the cache requests and responses that already passed
// the protection pipeline with zero findings.
//
// The cache is process-local and in-memory; it resets on restart,
// like channel health state (F1). It is not persisted and it is not
// shared between processes.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// DefaultTTL is how long a cached response stays valid. Cloudflare
// AI Gateway enforces a 60-second minimum and a one-month maximum;
// one hour is a conservative enterprise default for repeatable
// answers that do not change minute to minute.
const DefaultTTL = time.Hour

// DefaultMaxEntries bounds memory: cached bodies are small JSON
// documents, and the cap keeps the cache a cache rather than a
// second database.
const DefaultMaxEntries = 1000

// MaxBodyBytes is the largest response body that may be cached —
// the same 1 MiB cap as the REST adapter's upstream response limit
// (F7). Larger bodies are served but never stored.
const MaxBodyBytes = 1 << 20

type entry struct {
	status  int
	body    []byte
	expires time.Time
}

// Stats is a point-in-time snapshot for the admin console. Hits and
// misses are cumulative since process start; clearing entries does
// not reset them.
type Stats struct {
	Enabled    bool  `json:"enabled"`
	TTLSeconds int64 `json:"ttl_seconds"`
	MaxEntries int   `json:"max_entries"`
	Entries    int   `json:"entries"`
	Hits       int64 `json:"hits"`
	Misses     int64 `json:"misses"`
}

type Cache struct {
	mu         sync.Mutex
	enabled    bool
	ttl        time.Duration
	maxEntries int
	entries    map[string]*entry
	order      []string // insertion order; the front is evicted first
	hits       int64
	misses     int64
}

// New builds a cache that starts enabled. A non-positive ttl falls
// back to DefaultTTL; a non-positive maxEntries to DefaultMaxEntries.
func New(ttl time.Duration, maxEntries int) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &Cache{enabled: true, ttl: ttl, maxEntries: maxEntries, entries: map[string]*entry{}}
}

// Key builds the exact-match cache key: SHA-256 over the serving
// channel ID, the request's data level and the full request body.
// This mirrors Cloudflare AI Gateway (provider + endpoint + model +
// auth header + full body, SHA-256) and Portkey simple cache (exact
// match on the full request body). The channel ID is part of the key
// because the same body routed to a different channel is a different
// provider call, and two channels may legitimately answer
// differently; the data level is part of the key because routing —
// and therefore which channel is the first pick — depends on it.
func Key(channelID, dataLevel string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(channelID))
	h.Write([]byte{0})
	h.Write([]byte(dataLevel))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Enabled reports whether the cache is currently serving and
// storing entries.
func (c *Cache) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

// SetEnabled turns the cache on or off at runtime. Disabling stops
// both lookups and stores; stored entries are kept, so re-enabling
// serves them again until they expire. Clear removes them.
func (c *Cache) SetEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
}

// Get returns a stored response. An expired entry counts as a miss
// and is dropped. When the cache is disabled Get always misses and
// the miss is not counted — a disabled cache is invisible to the
// counters. The returned body is a copy; mutating it cannot corrupt
// the stored entry.
func (c *Cache) Get(key string) (int, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return 0, nil, false
	}
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		if ok {
			delete(c.entries, key)
		}
		c.misses++
		return 0, nil, false
	}
	c.hits++
	cp := make([]byte, len(e.body))
	copy(cp, e.body)
	return e.status, cp, true
}

// Put stores a response. Only 200 responses are stored; anything
// else (upstream errors, oversized bodies) is served but never
// cached. When the cache is full the oldest entries are evicted
// first (FIFO); re-storing an existing key refreshes its body and
// expiry without moving it in the eviction order.
func (c *Cache) Put(key string, status int, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || status != 200 || len(body) == 0 || len(body) > MaxBodyBytes {
		return
	}
	if _, ok := c.entries[key]; !ok {
		for len(c.entries) >= c.maxEntries && len(c.order) > 0 {
			old := c.order[0]
			c.order = c.order[1:]
			if _, live := c.entries[old]; live {
				delete(c.entries, old)
			}
		}
		c.order = append(c.order, key)
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	c.entries[key] = &entry{status: status, body: cp, expires: time.Now().Add(c.ttl)}
}

// Stats returns the current counters and size.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{Enabled: c.enabled, TTLSeconds: int64(c.ttl.Seconds()),
		MaxEntries: c.maxEntries, Entries: len(c.entries), Hits: c.hits, Misses: c.misses}
}

// Clear drops every stored entry and returns how many were dropped.
// Counters are kept: clearing is an admin action, not a reset of the
// usage record.
func (c *Cache) Clear() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.entries)
	c.entries = map[string]*entry{}
	c.order = nil
	return n
}
