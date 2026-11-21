package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func appendTestEvents(t *testing.T, l *Log) []Event {
	t.Helper()
	evs := []Event{
		{Actor: "alice", ActorID: "usr_1", Kind: "model_call", Action: "chat_completions", Model: "mock-eco"},
		{Actor: "bob", ActorID: "usr_2", Kind: "model_call", Action: "chat_completions", Model: "mock-eco"},
		{Actor: "alice", ActorID: "usr_1", Kind: "violation", Action: "model_call_blocked", Decision: "block"},
		{Actor: "alice", ActorID: "usr_1", Kind: "admin", Action: "channel_created"},
	}
	var out []Event
	for _, ev := range evs {
		out = append(out, l.Append(ev))
	}
	return out
}

func decodeNDJSON(t *testing.T, b []byte) []Event {
	t.Helper()
	var out []Event
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("export line is not valid JSON: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func TestExportAll(t *testing.T) {
	l := openTestLog(t)
	appendTestEvents(t, l)
	var buf bytes.Buffer
	sum, err := l.Export(&buf, ExportFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Count != 4 || sum.FirstSeq != 1 || sum.LastSeq != 4 {
		t.Fatalf("summary = %+v, want count 4 seq 1-4", sum)
	}
	if !sum.ChainValid {
		t.Fatal("expected ChainValid for untampered log")
	}
	evs := decodeNDJSON(t, buf.Bytes())
	if len(evs) != 4 {
		t.Fatalf("exported %d events, want 4", len(evs))
	}
	if evs[0].Seq != 1 || evs[3].Seq != 4 {
		t.Fatalf("export order is not oldest-first: seqs %d..%d", evs[0].Seq, evs[3].Seq)
	}
	if sum.HeadHash != evs[3].Hash {
		t.Fatalf("HeadHash %q != last event hash %q", sum.HeadHash, evs[3].Hash)
	}
	// Exported lines must re-verify as a chain on their own.
	if !verifyEvents(evs) {
		t.Fatal("exported events do not verify as a hash chain")
	}
}

func TestExportFilters(t *testing.T) {
	l := openTestLog(t)
	evs := appendTestEvents(t, l)
	// Half-open time window: [ev0.Time, ev2.Time) covers exactly the first two.
	var buf bytes.Buffer
	sum, err := l.Export(&buf, ExportFilter{Since: evs[0].Time, Until: evs[2].Time})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Count != 2 || sum.FirstSeq != 1 || sum.LastSeq != 2 {
		t.Fatalf("time window summary = %+v, want count 2 seq 1-2", sum)
	}
	// Kind filter.
	buf.Reset()
	sum, err = l.Export(&buf, ExportFilter{Kind: "model_call"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Count != 2 {
		t.Fatalf("kind filter exported %d events, want 2", sum.Count)
	}
	// Actor filter.
	buf.Reset()
	sum, err = l.Export(&buf, ExportFilter{Actor: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Count != 1 || sum.FirstSeq != 2 {
		t.Fatalf("actor filter summary = %+v, want count 1 seq 2", sum)
	}
	// Kind + actor combined.
	buf.Reset()
	sum, err = l.Export(&buf, ExportFilter{Kind: "admin", Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Count != 1 || sum.FirstSeq != 4 {
		t.Fatalf("combined filter summary = %+v, want count 1 seq 4", sum)
	}
}

func TestExportEmptyRange(t *testing.T) {
	l := openTestLog(t)
	appendTestEvents(t, l)
	var buf bytes.Buffer
	future := time.Now().UTC().Add(time.Hour)
	sum, err := l.Export(&buf, ExportFilter{Since: future})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Count != 0 || sum.FirstSeq != 0 || sum.LastSeq != 0 || sum.HeadHash != "" {
		t.Fatalf("empty export summary = %+v, want all zero", sum)
	}
	if !sum.ChainValid {
		t.Fatal("empty export of a valid log must still report ChainValid")
	}
	if buf.Len() != 0 {
		t.Fatalf("empty export wrote %d bytes, want 0", buf.Len())
	}
}

func TestExportDetectsBrokenChain(t *testing.T) {
	l := openTestLog(t)
	appendTestEvents(t, l)
	l.events[1].Action = "tampered_action"
	var buf bytes.Buffer
	sum, err := l.Export(&buf, ExportFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.ChainValid {
		t.Fatal("export of a tampered log must report ChainValid=false")
	}
	// The export itself still streams the events (evidence first);
	// the header is what flags it.
	if sum.Count != 4 {
		t.Fatalf("tampered export count = %d, want 4", sum.Count)
	}
}
