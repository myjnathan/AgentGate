package audit

import (
	"path/filepath"
	"testing"
)

func openTestLog(t *testing.T) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestVerifyValidChain(t *testing.T) {
	l := openTestLog(t)
	l.Append(Event{Actor: "alice", ActorID: "usr_1", Kind: "auth", Action: "login"})
	l.Append(Event{Actor: "alice", ActorID: "usr_1", Kind: "model_call", Action: "chat_completions", Model: "mock-eco"})
	l.Append(Event{Actor: "bob", ActorID: "usr_2", Kind: "tool_call", Action: "kb_search"})

	if !l.Verify() {
		t.Fatal("expected hash chain to verify for untampered log")
	}
	// Chain links: first event hangs off genesis, each next off the previous hash.
	if l.events[0].PrevHash != "genesis" {
		t.Fatalf("first event PrevHash = %q, want genesis", l.events[0].PrevHash)
	}
	for i := 1; i < len(l.events); i++ {
		if l.events[i].PrevHash != l.events[i-1].Hash {
			t.Fatalf("event %d PrevHash does not match event %d Hash", i, i-1)
		}
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	l := openTestLog(t)
	l.Append(Event{Actor: "alice", ActorID: "usr_1", Kind: "auth", Action: "login"})
	l.Append(Event{Actor: "alice", ActorID: "usr_1", Kind: "model_call", Action: "chat_completions", Model: "mock-eco"})
	l.Append(Event{Actor: "bob", ActorID: "usr_2", Kind: "tool_call", Action: "kb_search"})

	if !l.Verify() {
		t.Fatal("chain should verify before tampering")
	}
	// Rewrite history in memory: change an action without recomputing hashes.
	l.events[1].Action = "tampered_action"
	if l.Verify() {
		t.Fatal("expected Verify to fail after tampering with an event")
	}
}

func TestVerifyDetectsBrokenLink(t *testing.T) {
	l := openTestLog(t)
	l.Append(Event{Actor: "alice", Kind: "auth", Action: "login"})
	l.Append(Event{Actor: "alice", Kind: "auth", Action: "logout"})

	// Sever the chain link while keeping the event's own hash consistent
	// is impossible without the private recompute; simply corrupting
	// PrevHash must be caught.
	l.events[1].PrevHash = "deadbeef"
	if l.Verify() {
		t.Fatal("expected Verify to fail after breaking PrevHash link")
	}
}

func TestQueryFiltersByKindAndActor(t *testing.T) {
	l := openTestLog(t)
	l.Append(Event{Actor: "alice", Kind: "model_call", Action: "chat_completions"})
	l.Append(Event{Actor: "bob", Kind: "model_call", Action: "chat_completions"})
	l.Append(Event{Actor: "alice", Kind: "tool_call", Action: "kb_search"})
	l.Append(Event{Actor: "alice", Kind: "violation", Action: "model_call_blocked"})

	// No filter: all events, newest first.
	all := l.Query("", "", 100)
	if len(all) != 4 {
		t.Fatalf("unfiltered query returned %d events, want 4", len(all))
	}
	if all[0].Kind != "violation" {
		t.Fatalf("newest event first: got kind %q, want violation", all[0].Kind)
	}

	// Filter by kind.
	calls := l.Query("model_call", "", 100)
	if len(calls) != 2 {
		t.Fatalf("kind=model_call returned %d events, want 2", len(calls))
	}
	for _, ev := range calls {
		if ev.Kind != "model_call" {
			t.Fatalf("kind filter leaked event of kind %q", ev.Kind)
		}
	}

	// Filter by actor.
	alice := l.Query("", "alice", 100)
	if len(alice) != 3 {
		t.Fatalf("actor=alice returned %d events, want 3", len(alice))
	}
	for _, ev := range alice {
		if ev.Actor != "alice" {
			t.Fatalf("actor filter leaked event by %q", ev.Actor)
		}
	}

	// Filter by both kind and actor.
	both := l.Query("tool_call", "alice", 100)
	if len(both) != 1 || both[0].Action != "kb_search" {
		t.Fatalf("kind+actor query = %+v, want the single kb_search event", both)
	}

	// No match.
	if got := l.Query("tool_call", "bob", 100); len(got) != 0 {
		t.Fatalf("non-matching query returned %d events, want 0", len(got))
	}
}
