// Full-history verification (backlog F27 phase 3 B1).
//
// VerifyFullHistory checks the whole audit history held in a data
// directory: every archive segment (audit-archive-<first>-<last>.jsonl.gz
// plus its .manifest.json sidecar) in sequence order, followed by the
// live log (audit.jsonl). Checkpoint lines are skipped everywhere — a
// checkpoint is a marker, not a link: the events it anchors link from
// the archived head hash, so the concatenated non-checkpoint stream
// must verify as one hash chain. A segment's manifest must agree with
// the segment bytes (event count, first/last seq, head hash) and claim
// chain_valid, so a missing, reordered or edited segment fails
// closed. An empty directory (no segments, no live log) verifies
// vacuously with zero counts.
//
// This is what the backup restore path runs after downloading state
// and segments ("take it back and verify the chain"): a restore that
// cannot prove the full chain is refused, never silently accepted.
package audit

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// HistoryReport is the outcome of VerifyFullHistory.
type HistoryReport struct {
	ChainValid    bool `json:"chain_valid"`
	Segments      int  `json:"segments"`
	SegmentEvents int  `json:"segment_events"`
	LiveEvents    int  `json:"live_events"`
	TotalEvents   int  `json:"total_events"`
}

// VerifyFullHistory verifies the concatenated audit history in dir:
// archive segments in first-seq order, then the live log, skipping
// checkpoint lines. It returns an error only when the history cannot
// be read or a manifest disagrees with its segment; a broken chain
// is reported as ChainValid false, not an error, so callers can
// distinguish "unreadable" from "tampered".
func VerifyFullHistory(dir string) (HistoryReport, error) {
	var rep HistoryReport
	names, err := filepath.Glob(filepath.Join(dir, "audit-archive-*.jsonl.gz"))
	if err != nil {
		return rep, err
	}
	type segment struct {
		first int64
		last  int64
		path  string
	}
	var segs []segment
	for _, name := range names {
		first, last, ok := parseArchiveName(filepath.Base(name))
		if !ok {
			return rep, fmt.Errorf("unparsable archive name %q", filepath.Base(name))
		}
		segs = append(segs, segment{first: first, last: last, path: name})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].first < segs[j].first })
	var all []Event
	for _, sg := range segs {
		events, man, err := readArchiveSegment(sg.path)
		if err != nil {
			return rep, err
		}
		if !man.ChainValid {
			return rep, fmt.Errorf("archive %s manifest claims chain invalid", filepath.Base(sg.path))
		}
		if man.FirstSeq != sg.first || man.LastSeq != sg.last {
			return rep, fmt.Errorf("archive %s manifest seq %d-%d != file name %d-%d",
				filepath.Base(sg.path), man.FirstSeq, man.LastSeq, sg.first, sg.last)
		}
		if man.EventCount != len(events) {
			return rep, fmt.Errorf("archive %s manifest counts %d events, file holds %d",
				filepath.Base(sg.path), man.EventCount, len(events))
		}
		if len(events) == 0 {
			return rep, fmt.Errorf("archive %s holds no events", filepath.Base(sg.path))
		}
		if events[0].Seq != sg.first || events[len(events)-1].Seq != sg.last {
			return rep, fmt.Errorf("archive %s holds seq %d-%d, want %d-%d",
				filepath.Base(sg.path), events[0].Seq, events[len(events)-1].Seq, sg.first, sg.last)
		}
		if man.HeadHash != events[len(events)-1].Hash {
			return rep, fmt.Errorf("archive %s manifest head hash disagrees with last event", filepath.Base(sg.path))
		}
		rep.Segments++
		rep.SegmentEvents += len(events)
		for _, ev := range events {
			if !isCheckpoint(ev) {
				all = append(all, ev)
			}
		}
	}
	live, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		return rep, err
	}
	for _, ev := range live.All() {
		if isCheckpoint(ev) {
			continue
		}
		all = append(all, ev)
		rep.LiveEvents++
	}
	rep.TotalEvents = len(all)
	rep.ChainValid = verifyEvents(all)
	return rep, nil
}

// parseArchiveName splits audit-archive-<first>-<last>.jsonl.gz into
// its sequence bounds.
func parseArchiveName(name string) (first, last int64, ok bool) {
	rest, found := strings.CutPrefix(name, "audit-archive-")
	if !found {
		return 0, 0, false
	}
	rest, found = strings.CutSuffix(rest, ".jsonl.gz")
	if !found {
		return 0, 0, false
	}
	a, b, found := strings.Cut(rest, "-")
	if !found {
		return 0, 0, false
	}
	first, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || first <= 0 || last < first {
		return 0, 0, false
	}
	return first, last, true
}

// readArchiveSegment reads one archive file plus its manifest sidecar.
func readArchiveSegment(path string) ([]Event, ArchiveManifest, error) {
	var man ArchiveManifest
	base := strings.TrimSuffix(path, ".jsonl.gz")
	manifestBytes, err := os.ReadFile(base + ".manifest.json")
	if err != nil {
		return nil, man, fmt.Errorf("archive %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(manifestBytes, &man); err != nil {
		return nil, man, fmt.Errorf("archive %s manifest: %w", filepath.Base(path), err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, man, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, man, fmt.Errorf("archive %s: %w", filepath.Base(path), err)
	}
	defer gz.Close()
	var events []Event
	dec := json.NewDecoder(bufio.NewReader(gz))
	for dec.More() {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			return nil, man, fmt.Errorf("archive %s: %w", filepath.Base(path), err)
		}
		events = append(events, ev)
	}
	return events, man, nil
}
