package audit

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyFullHistoryNoArchive(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	appendTestEvents(t, l)
	rep, err := VerifyFullHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ChainValid || rep.Segments != 0 || rep.SegmentEvents != 0 || rep.LiveEvents != 4 || rep.TotalEvents != 4 {
		t.Fatalf("report = %+v, want valid, 0 segments, 4 live, 4 total", rep)
	}
}

func TestVerifyFullHistoryTwoArchives(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	l.SetRetentionDays(180)
	evs := appendTestEvents(t, l)
	// First archive: events 1-2 out, checkpoint + 3-4 stay.
	if _, err := l.Archive(evs[1].Time.Add(time.Nanosecond), "admin"); err != nil {
		t.Fatal(err)
	}
	// Second archive with a cutoff after the first checkpoint's time:
	// the checkpoint itself moves into segment 2 (checkpoint lines
	// must be skipped when the history is concatenated, or the
	// chain would fail on the checkpoint's "genesis" prev-hash).
	if _, err := l.Archive(time.Now().Add(time.Second), "admin"); err != nil {
		t.Fatal(err)
	}
	rep, err := VerifyFullHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Segment 1 holds 2 events, segment 2 holds checkpoint + 2 events,
	// the live log holds only the second checkpoint: 4 real events.
	if !rep.ChainValid || rep.Segments != 2 || rep.SegmentEvents != 5 || rep.LiveEvents != 0 || rep.TotalEvents != 4 {
		t.Fatalf("report = %+v, want valid, 2 segments, 5 segment events (incl. checkpoint), 0 live, 4 total", rep)
	}
}

func TestVerifyFullHistoryTamperedSegment(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	evs := appendTestEvents(t, l)
	rep, err := l.Archive(evs[1].Time.Add(time.Nanosecond), "admin")
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the segment with one event's action changed: the
	// manifest still agrees (counts, seq bounds and head hash are
	// untouched — only the first event's body changed), so the
	// failure must surface as a broken chain, not a read error.
	rewriteSegmentEvent(t, filepath.Join(dir, rep.ArchiveFile), 0, func(ev *Event) {
		ev.Action = "channel_deleted"
	})
	rep2, err := VerifyFullHistory(dir)
	if err != nil {
		t.Fatalf("tampered segment should report chain invalid, not an error: %v", err)
	}
	if rep2.ChainValid {
		t.Fatalf("tampered segment verified valid: %+v", rep2)
	}
}

func TestVerifyFullHistoryTamperedLiveLog(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	appendTestEvents(t, l)
	path := filepath.Join(dir, "audit.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	var ev Event
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	ev.Action = "channel_deleted"
	b, _ := json.Marshal(ev)
	lines[0] = string(b)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := VerifyFullHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ChainValid {
		t.Fatalf("tampered live log verified valid: %+v", rep)
	}
}

func TestVerifyFullHistoryMissingManifest(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	evs := appendTestEvents(t, l)
	rep, err := l.Archive(evs[1].Time.Add(time.Nanosecond), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, rep.ManifestFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFullHistory(dir); err == nil {
		t.Fatal("missing manifest must fail verification")
	}
}

func TestVerifyFullHistoryEmptyDir(t *testing.T) {
	rep, err := VerifyFullHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ChainValid || rep.TotalEvents != 0 {
		t.Fatalf("empty dir report = %+v, want valid with 0 events", rep)
	}
}

// TestAnchorCheckpointContinuesChain simulates a B1 restore: the
// live log is gone, only segments remain. The restored log starts
// with an anchor checkpoint at the last segment's head; new events
// must continue the chain (seq past the archived head, prev-hash the
// archived head), and the full history must still verify after the
// restored log is archived again.
func TestAnchorCheckpointContinuesChain(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	evs := appendTestEvents(t, l)
	rep, err := l.Archive(evs[1].Time.Add(time.Nanosecond), "admin")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate volume loss: only the segment + manifest survive.
	livePath := filepath.Join(dir, "audit.jsonl")
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	// Restore: anchor the fresh live log at the segment head.
	anchor := AnchorCheckpoint(rep.HeadHash, rep.LastSeq, rep.ArchiveFile, "system")
	b, _ := json.Marshal(anchor)
	if err := os.WriteFile(livePath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(livePath)
	if err != nil {
		t.Fatal(err)
	}
	ev := l2.Append(Event{Actor: "alice", Kind: "model_call", Action: "chat_completions"})
	if ev.Seq != rep.LastSeq+1 || ev.PrevHash != rep.HeadHash {
		t.Fatalf("restored event = seq %d prev %q, want seq %d prev %q",
			ev.Seq, ev.PrevHash, rep.LastSeq+1, rep.HeadHash)
	}
	if !l2.Verify() {
		t.Fatal("restored live log does not verify")
	}
	// Archive the restored log again (cutoff past the anchor) and
	// verify the whole history: the 2 archived events survive, the
	// 2 live events that died with the volume are honestly gone
	// (B1 backs up archive segments, not the live log), plus the 1
	// new event written after the restore.
	if _, err := l2.Archive(time.Now().Add(time.Second), "admin"); err != nil {
		t.Fatal(err)
	}
	full, err := VerifyFullHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !full.ChainValid || full.TotalEvents != 3 {
		t.Fatalf("full history after restore+rearchive = %+v, want valid, 3 events", full)
	}
}

// rewriteSegmentEvent decodes a gzip archive, applies fn to the
// event at index, and writes it back (recomputing nothing, so the
// stored manifest head hash no longer matches the bytes).
func rewriteSegmentEvent(t *testing.T, path string, index int, fn func(ev *Event)) {
	t.Helper()
	events := readGzipEvents(t, path)
	if index >= len(events) {
		t.Fatalf("index %d out of range (%d events)", index, len(events))
	}
	fn(&events[index])
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
