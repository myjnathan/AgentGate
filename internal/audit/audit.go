// Package audit is an append-only, hash-chained audit log.
// Each event links to the previous event's hash, so tampering with
// history is detectable. Events never contain plaintext secrets or
// detected sensitive values — only categories, kinds and metadata.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Event struct {
	Seq       int64             `json:"seq"`
	Time      time.Time         `json:"time"`
	Actor     string            `json:"actor"` // username
	ActorID   string            `json:"actor_id"`
	AgentType string            `json:"agent_type,omitempty"`
	Tier      string            `json:"tier,omitempty"`
	Kind      string            `json:"kind"` // model_call | tool_call | admin | auth | violation
	Action    string            `json:"action"`
	Model     string            `json:"model,omitempty"`
	Channel   string            `json:"channel,omitempty"`
	DataLevel string            `json:"data_level,omitempty"`
	Decision  string            `json:"decision,omitempty"` // allow | warn | redact | block | reroute
	Detail    map[string]string `json:"detail,omitempty"`
	Tokens    int64             `json:"tokens,omitempty"`
	// CostUSD is the cost of a model_call in USD, priced at call time
	// from the serving channel's per-model price (backlog F13).
	// Zero means unpriced (no price configured) or no usage; stats
	// report such calls separately as unpriced_calls.
	CostUSD  float64 `json:"cost_usd,omitempty"`
	PrevHash string  `json:"prev_hash"`
	Hash     string  `json:"hash"`
}

type Log struct {
	mu            sync.Mutex
	path          string
	seq           int64
	lastHash      string
	retentionDays int     // configured retention policy (days); 0 = unset
	events        []Event // in-memory tail for queries (all events, night-one scale)
	notify        func(Event)
	// lastErr is the error from the most recent append write, or nil
	// when the last append reached disk. Append used to swallow write
	// errors silently, so a full disk looked healthy; /readyz (backlog
	// F27) reads this to drain a node whose audit log is unwritable.
	lastErr error
}

// SetNotifyHook registers a callback invoked after every appended
// event (outside the log lock). AgentGate wires the alert dispatcher
// here (backlog F5): the hook only reads the event, and alertable
// kinds exclude Kind "alert", so delivery reports cannot re-trigger
// delivery.
func (l *Log) SetNotifyHook(fn func(Event)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notify = fn
}

// chainHeadHash returns the hash the next appended event must link
// from: normally the last event's hash, but when the log ends with an
// archive checkpoint the archived segment's head hash — the
// checkpoint is a marker, not a link, and the chain continues from
// the archived head (see verifyEvents).
func chainHeadHash(events []Event) string {
	if len(events) == 0 {
		return "genesis"
	}
	last := events[len(events)-1]
	if isCheckpoint(last) {
		if h, ok := last.Detail["archived_head_hash"]; ok && h != "" {
			return h
		}
	}
	return last.Hash
}

func Open(path string) (*Log, error) {
	l := &Log{path: path, lastHash: "genesis"}
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			var ev Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				l.events = append(l.events, ev)
				l.seq = ev.Seq
			}
		}
		l.lastHash = chainHeadHash(l.events)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return l, nil
}

func hashEvent(ev Event) string {
	ev.Hash = ""
	b, _ := json.Marshal(ev)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Append records an event and returns it with its hash assigned.
// The notify hook (if any) runs after the lock is released, so a hook
// that appends its own event cannot deadlock.
func (l *Log) Append(ev Event) Event {
	l.mu.Lock()
	l.seq++
	ev.Seq = l.seq
	ev.Time = time.Now().UTC()
	ev.PrevHash = l.lastHash
	ev.Hash = hashEvent(ev)
	l.lastHash = ev.Hash
	l.events = append(l.events, ev)
	notify := l.notify
	if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(ev)
		if _, err := f.Write(append(b, '\n')); err != nil {
			l.lastErr = err
		} else {
			l.lastErr = nil
		}
		_ = f.Close()
	} else {
		l.lastErr = err
	}
	l.mu.Unlock()
	if notify != nil {
		notify(ev)
	}
	return ev
}

// LastError returns the error from the most recent append write, or
// nil when the last append reached disk. A failed append still records
// the event in memory (and in the hash chain), so the in-memory tail
// and the file can diverge until the disk recovers.
func (l *Log) LastError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastErr
}

// All returns a copy of all recorded events, oldest first.
// Stats aggregation reads from this same log, so reported numbers
// reconcile with the audit trail by construction.
func (l *Log) All() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

// Query returns events matching the filters, newest first, up to limit.
func (l *Log) Query(kind, actor string, limit int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []Event
	for i := len(l.events) - 1; i >= 0 && len(out) < limit; i-- {
		ev := l.events[i]
		if kind != "" && ev.Kind != kind {
			continue
		}
		if actor != "" && ev.Actor != actor {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// Verify checks the hash chain integrity. A checkpoint event
// (written by Archive) re-anchors the chain at the archived segment's
// head hash; see verifyEvents.
func (l *Log) Verify() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return verifyEvents(l.events)
}
