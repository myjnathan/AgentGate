package audit

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readGzipEvents(t *testing.T, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var out []Event
	dec := json.NewDecoder(gz)
	for dec.More() {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func TestArchiveMovesOldEvents(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	l.SetRetentionDays(180)
	evs := appendTestEvents(t, l)
	// Archive everything before just after the second event: the first
	// two events move out, the last two stay.
	cutoff := evs[1].Time.Add(time.Nanosecond)
	rep, err := l.Archive(cutoff, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Archived != 2 || rep.FirstSeq != 1 || rep.LastSeq != 2 {
		t.Fatalf("report = %+v, want archived 2 seq 1-2", rep)
	}
	if rep.ArchiveFile == "" || rep.ManifestFile == "" {
		t.Fatalf("report missing file names: %+v", rep)
	}
	if !rep.ChainValid || rep.RetentionDays != 180 {
		t.Fatalf("report = %+v, want chain_valid and retention 180", rep)
	}
	if rep.LiveEvents != 3 { // checkpoint + 2 kept
		t.Fatalf("live events = %d, want 3 (checkpoint + 2 kept)", rep.LiveEvents)
	}
	// Archive file holds the two old events verbatim, chain intact.
	archived := readGzipEvents(t, filepath.Join(dir, rep.ArchiveFile))
	if len(archived) != 2 || archived[0].Seq != 1 || archived[1].Seq != 2 {
		t.Fatalf("archive holds %+v, want seq 1-2", archived)
	}
	if archived[1].Hash != rep.HeadHash {
		t.Fatalf("report HeadHash %q != last archived hash %q", rep.HeadHash, archived[1].Hash)
	}
	// Manifest agrees with the archive.
	manifestBytes, err := os.ReadFile(filepath.Join(dir, rep.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var man ArchiveManifest
	if err := json.Unmarshal(manifestBytes, &man); err != nil {
		t.Fatal(err)
	}
	if man.EventCount != 2 || man.HeadHash != rep.HeadHash || !man.ChainValid || man.ArchiveFile != rep.ArchiveFile {
		t.Fatalf("manifest = %+v, want count 2, head %q, valid", man, rep.HeadHash)
	}
	// Live log still verifies, and starts with the checkpoint anchor.
	if !l.Verify() {
		t.Fatal("live log must verify after archiving")
	}
	live := l.All()
	if len(live) != 3 || live[0].Action != CheckpointAction || live[0].PrevHash != "genesis" {
		t.Fatalf("live log head = %+v, want checkpoint with PrevHash genesis", live[0])
	}
	if live[0].Detail["archived_head_hash"] != rep.HeadHash {
		t.Fatalf("checkpoint archived_head_hash = %q, want %q", live[0].Detail["archived_head_hash"], rep.HeadHash)
	}
	if live[0].Detail["archive_file"] != rep.ArchiveFile {
		t.Fatalf("checkpoint archive_file = %q, want %q", live[0].Detail["archive_file"], rep.ArchiveFile)
	}
	if live[1].Seq != 3 || live[2].Seq != 4 {
		t.Fatalf("kept events seqs = %d,%d, want 3,4", live[1].Seq, live[2].Seq)
	}
	// Offline full-history check: archive events + live events
	// (skipping checkpoint lines) verify as one chain.
	var full []Event
	full = append(full, archived...)
	for _, ev := range live {
		if isCheckpoint(ev) {
			continue
		}
		full = append(full, ev)
	}
	if !verifyEvents(full) {
		t.Fatal("archive + live (minus checkpoint) must verify as one chain")
	}
	// Reopening the pruned log from disk keeps working, and new
	// appends continue the sequence after the kept events.
	l2, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !l2.Verify() {
		t.Fatal("reopened pruned log must verify")
	}
	nv := l2.Append(Event{Actor: "carol", Kind: "auth", Action: "login"})
	if nv.Seq != 5 {
		t.Fatalf("append after archive got seq %d, want 5", nv.Seq)
	}
	if !l2.Verify() {
		t.Fatal("log must verify after appending post-archive")
	}
}

func TestArchiveNoOpWhenNothingOld(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	appendTestEvents(t, l)
	rep, err := l.Archive(time.Now().UTC().Add(-time.Hour), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Archived != 0 || rep.ArchiveFile != "" {
		t.Fatalf("no-op archive report = %+v, want archived 0, no file", rep)
	}
	if len(l.All()) != 4 {
		t.Fatalf("no-op archive changed the log: %d events", len(l.All()))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("no-op archive wrote extra files in %s", dir)
	}
}

func TestArchiveRefusesBrokenChain(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	appendTestEvents(t, l)
	l.events[1].Action = "tampered_action"
	if _, err := l.Archive(time.Now().UTC().Add(time.Hour), "admin"); err == nil {
		t.Fatal("archive of a tampered log must fail")
	}
	if len(l.All()) != 4 {
		t.Fatalf("failed archive changed the log: %d events", len(l.All()))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("failed archive wrote files in %s", dir)
	}
}

func TestArchiveTwiceKeepsChainValid(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	evs := appendTestEvents(t, l)
	if _, err := l.Archive(evs[1].Time.Add(time.Nanosecond), "admin"); err != nil {
		t.Fatal(err)
	}
	l.Append(Event{Actor: "carol", Kind: "model_call", Action: "chat_completions"})
	live := l.All()
	rep, err := l.Archive(live[len(live)-1].Time.Add(time.Nanosecond), "admin")
	if err != nil {
		t.Fatal(err)
	}
	// Second archive took the old checkpoint plus the kept events.
	if rep.Archived != 4 {
		t.Fatalf("second archive archived %d events, want 4 (checkpoint + 3)", rep.Archived)
	}
	if !l.Verify() {
		t.Fatal("live log must verify after the second archive")
	}
	if len(l.All()) != 1 || l.All()[0].Action != CheckpointAction {
		t.Fatalf("live log after archiving everything = %+v, want a single checkpoint", l.All())
	}
	nv := l.Append(Event{Actor: "dave", Kind: "auth", Action: "login"})
	if nv.Seq != 6 {
		t.Fatalf("append after full archive got seq %d, want 6", nv.Seq)
	}
	if !l.Verify() {
		t.Fatal("log must verify after appending post full-archive")
	}
}
