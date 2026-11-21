// Package ratelimit enforces per-token RPM/TPM limits for the model
// gateway (backlog F3).
//
// Design mirrors LiteLLM's per-key limits (rpm_limit / tpm_limit on
// /key/generate, docs.litellm.ai): a 60-second sliding window per
// token, 429 when the window is exhausted, and token usage recorded
// only after a response completes — requests already in flight are
// never blocked by tokens they have not yet consumed. A limit of 0
// means "unlimited" for that dimension.
package ratelimit

import (
	"sync"
	"time"
)

// Window is the sliding-window length for both RPM and TPM.
const Window = time.Minute

type tokenUse struct {
	at     time.Time
	tokens int64
}

type bucket struct {
	requests []time.Time
	uses     []tokenUse
}

// Limiter keeps one sliding-window bucket per token ID.
// The zero value is not usable; use New.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

// New returns an empty Limiter.
func New() *Limiter {
	return &Limiter{buckets: map[string]*bucket{}}
}

func (l *Limiter) bucket(key string) *bucket {
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{}
		l.buckets[key] = b
	}
	return b
}

// prune drops entries at or before the window start.
func prune(b *bucket, now time.Time) {
	cutoff := now.Add(-Window)
	keep := b.requests[:0]
	for _, t := range b.requests {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	b.requests = keep
	keepUses := b.uses[:0]
	for _, u := range b.uses {
		if u.at.After(cutoff) {
			keepUses = append(keepUses, u)
		}
	}
	b.uses = keepUses
}

func tokensUsed(b *bucket) int64 {
	var n int64
	for _, u := range b.uses {
		n += u.tokens
	}
	return n
}

// Allow checks the RPM and TPM limits for key at now. When allowed,
// the request itself is counted toward RPM immediately; its tokens
// are recorded later via RecordTokens, once the upstream usage is
// known. When denied, nothing is counted.
//
// The second return value names the violated dimension ("rpm" or
// "tpm"); the third is how long the caller should wait before
// retrying (0 when allowed).
func (l *Limiter) Allow(key string, rpm, tpm int, now time.Time) (bool, string, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(key)
	prune(b, now)
	if rpm > 0 && len(b.requests) >= rpm {
		return false, "rpm", Window - now.Sub(b.requests[0])
	}
	if tpm > 0 && tokensUsed(b) >= int64(tpm) {
		return false, "tpm", Window - now.Sub(b.uses[0].at)
	}
	b.requests = append(b.requests, now)
	return true, "", 0
}

// RecordTokens adds n consumed tokens to key's current window.
// Call it once per completed request, with the upstream-reported
// usage (0 is a no-op).
func (l *Limiter) RecordTokens(key string, n int64, now time.Time) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(key)
	prune(b, now)
	b.uses = append(b.uses, tokenUse{at: now, tokens: n})
}
