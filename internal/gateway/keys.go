// Key rotation for multi-key channels (backlog F12).
//
// Selection follows QuantumNous/new-api's GetNextEnabledKey
// (model/channel.go, AGPL-3.0; reused under Nathan's 2026-11-21
// reuse-first instruction — AGPL-3.0 is compatible with this project's
// AGPL-3.0, original copyright retained by the upstream project): one
// lock per channel, a polling index pointing at the next candidate, a
// ring scan that skips disabled keys, and an explicit error when no key
// is enabled (never silently fall back to a disabled key — new-api
// fixed exactly that bug). Differences from new-api: we only implement
// polling (no random mode); the polling index is runtime-only (like F1
// channel health), not persisted; keys are stored encrypted one by one
// instead of newline-joined in a single string, so each key carries
// its own disabled state instead of a parallel status map.

package gateway

import (
	"strconv"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// nextKey picks the next enabled key of ch in round-robin order and
// returns its index and encrypted value, advancing the cursor before
// returning — even if this request later fails, the next request starts
// at the following key (new-api's rule). ok is false when the channel
// has no enabled key left.
func (g *Gateway) nextKey(ch *store.Channel) (idx int, enc string, ok bool) {
	g.keyMu.Lock()
	defer g.keyMu.Unlock()
	var enabled []int
	var encs []string
	g.st.Read(func(st *store.State) {
		c, found := st.Channels[ch.ID]
		if !found {
			return
		}
		for i, k := range c.Keys {
			if !k.Disabled {
				enabled = append(enabled, i)
				encs = append(encs, k.Enc)
			}
		}
	})
	if len(enabled) == 0 {
		return 0, "", false
	}
	n := ch.KeyCount()
	start := g.keyNext[ch.ID]
	if start < 0 || start >= n {
		start = 0
	}
	// Ring scan from the cursor: the first enabled key at or after it
	// wins, and the cursor moves past the winner.
	for i := 0; i < n; i++ {
		cand := (start + i) % n
		for j, e := range enabled {
			if e == cand {
				g.keyNext[ch.ID] = (cand + 1) % n
				return cand, encs[j], true
			}
		}
	}
	// Unreachable when enabled is non-empty, but never return a
	// disabled key: fall back to the first enabled one.
	g.keyNext[ch.ID] = (enabled[0] + 1) % n
	return enabled[0], encs[0], true
}

// disableKey marks key idx of ch disabled (reason recorded) and audits
// the removal. The audit event is classified by alert.FromEvent (F5),
// so a configured webhook is notified. The key itself is never logged —
// only its index and masked form, both already visible in the console.
func (g *Gateway) disableKey(ch *store.Channel, idx int, reason string) {
	g.keyMu.Lock()
	defer g.keyMu.Unlock()
	var masked string
	g.st.WithLock(func(st *store.State) {
		c, found := st.Channels[ch.ID]
		if !found || idx < 0 || idx >= len(c.Keys) {
			return
		}
		if c.Keys[idx].Disabled {
			return
		}
		c.Keys[idx].Disabled = true
		c.Keys[idx].DisabledReason = reason
		c.Keys[idx].DisabledAt = time.Now()
		masked = c.Keys[idx].Masked
	})
	g.audit.Append(audit.Event{Actor: "system", Kind: "channel", Action: "channel_key_disabled",
		Channel: ch.Name, Decision: "disable",
		Detail: map[string]string{"channel_id": ch.ID, "key_index": strconv.Itoa(idx),
			"key_masked": masked, "reason": reason}})
}
