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
	PrevHash  string            `json:"prev_hash"`
	Hash      string            `json:"hash"`
}

type Log struct {
	mu       sync.Mutex
	path     string
	seq      int64
	lastHash string
	events   []Event // in-memory tail for queries (all events, night-one scale)
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
				l.lastHash = ev.Hash
			}
		}
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
func (l *Log) Append(ev Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	ev.Seq = l.seq
	ev.Time = time.Now().UTC()
	ev.PrevHash = l.lastHash
	ev.Hash = hashEvent(ev)
	l.lastHash = ev.Hash
	l.events = append(l.events, ev)
	if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(ev)
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	return ev
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

// Verify checks the hash chain integrity.
func (l *Log) Verify() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := "genesis"
	for _, ev := range l.events {
		if ev.PrevHash != prev {
			return false
		}
		if hashEvent(ev) != ev.Hash {
			return false
		}
		prev = ev.Hash
	}
	return true
}
