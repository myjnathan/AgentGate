// Audit archiving and retention (F4).
//
// Archive moves events older than a cutoff out of the live log into a
// gzip-compressed NDJSON file (audit-archive-<first>-<last>.jsonl.gz)
// plus a JSON manifest sidecar, and leaves a checkpoint event as the
// new head of the live log. The checkpoint records the archived
// segment's head hash, so the live chain stays verifiable without
// rewriting history: Verify treats a checkpoint as an anchor whose
// archived_head_hash is what the next event must link from. Offline
// verification of the whole history is archive segments concatenated
// (skipping checkpoint lines) followed by the live log (skipping
// checkpoint lines); the manifest's chain_valid is the authoritative
// statement that the full chain verified at archive time.
//
// Retention is a policy, not a timer: AGENTGATE_AUDIT_RETENTION_DAYS
// (default 180) documents how long the live log keeps events, and an
// operator (or a cron job calling POST /api/audit/archive) archives
// everything older than now minus that many days. Archiving refuses
// to run when the chain is broken, so a tampered log can never be
// quietly pruned away.
package audit

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ErrChainInvalid is returned by Archive when the hash chain does not
// verify: archiving must never destroy evidence of tampering.
var ErrChainInvalid = errors.New("audit chain invalid; refusing to archive")

// CheckpointAction marks the anchor event Archive writes at the head
// of the pruned live log.
const CheckpointAction = "audit_archive_checkpoint"

// AnchorCheckpoint builds the checkpoint event that re-anchors a
// live log at an archived segment's head (backlog F27 phase 3 B1):
// it is the same event Archive writes when it prunes the live log —
// PrevHash "genesis", Seq the archived head's seq, Detail carrying
// the archived head hash and file. A restored log starts with this
// event, so new events link from the archived head and sequence
// numbers never collide with the archived segments; without it a
// restored server would restart the chain at genesis and the full
// history (segments + new log) would no longer verify as one chain.
// The "restored" marker distinguishes a restore anchor from an
// archive checkpoint in the audit trail.
func AnchorCheckpoint(headHash string, headSeq int64, archiveFile, actor string) Event {
	cp := Event{
		Seq:      headSeq,
		Time:     time.Now().UTC(),
		Actor:    actor,
		Kind:     "admin",
		Action:   CheckpointAction,
		Decision: "allow",
		Detail: map[string]string{
			"archive_file":       archiveFile,
			"archived_last_seq":  strconv.FormatInt(headSeq, 10),
			"archived_head_hash": headHash,
			"restored":           "true",
		},
		PrevHash: "genesis",
	}
	cp.Hash = hashEvent(cp)
	return cp
}

// ArchiveManifest is the JSON sidecar written next to each archive
// file, so an auditor can check an archive without opening it.
type ArchiveManifest struct {
	ArchiveFile   string    `json:"archive_file"`
	FirstSeq      int64     `json:"first_seq"`
	LastSeq       int64     `json:"last_seq"`
	FirstTime     time.Time `json:"first_time"`
	LastTime      time.Time `json:"last_time"`
	EventCount    int       `json:"event_count"`
	HeadHash      string    `json:"head_hash"`
	ChainValid    bool      `json:"chain_valid"`
	RetentionDays int       `json:"retention_days"`
	CreatedAt     time.Time `json:"created_at"`
}

// ArchiveReport is the result of an Archive call.
type ArchiveReport struct {
	ArchiveFile   string `json:"archive_file"`
	ManifestFile  string `json:"manifest_file"`
	Archived      int    `json:"archived"`
	FirstSeq      int64  `json:"first_seq"`
	LastSeq       int64  `json:"last_seq"`
	HeadHash      string `json:"head_hash"`
	ChainValid    bool   `json:"chain_valid"`
	RetentionDays int    `json:"retention_days"`
	LiveEvents    int    `json:"live_events"`
}

// SetRetentionDays records the configured retention policy (days) on
// the log; it is stamped into archive manifests and reports.
func (l *Log) SetRetentionDays(days int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.retentionDays = days
}

func isCheckpoint(ev Event) bool {
	return ev.Kind == "admin" && ev.Action == CheckpointAction
}

// verifyEvents checks the hash chain of an event slice. A checkpoint
// event links from the previous expected hash like any event, then
// re-anchors the chain: the following event must link from the
// checkpoint's archived_head_hash instead of the checkpoint's own
// hash, because the events between them live in the archive file.
func verifyEvents(events []Event) bool {
	prev := "genesis"
	for _, ev := range events {
		if ev.PrevHash != prev {
			return false
		}
		if hashEvent(ev) != ev.Hash {
			return false
		}
		prev = ev.Hash
		if isCheckpoint(ev) {
			if h, ok := ev.Detail["archived_head_hash"]; ok && h != "" {
				prev = h
			}
		}
	}
	return true
}

// Archive moves every event older than before into a gzip NDJSON
// archive plus manifest, and rewrites the live log to a checkpoint
// event followed by the remaining events. Nothing is archived when no
// event predates before; the chain must verify or the call fails
// without touching any file.
func (l *Log) Archive(before time.Time, actor string) (ArchiveReport, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !verifyEvents(l.events) {
		return ArchiveReport{}, ErrChainInvalid
	}
	var old, keep []Event
	for _, ev := range l.events {
		if ev.Time.Before(before) {
			old = append(old, ev)
		} else {
			keep = append(keep, ev)
		}
	}
	rep := ArchiveReport{ChainValid: true, RetentionDays: l.retentionDays, LiveEvents: len(l.events)}
	if len(old) == 0 {
		return rep, nil
	}
	dir := filepath.Dir(l.path)
	base := fmt.Sprintf("audit-archive-%d-%d", old[0].Seq, old[len(old)-1].Seq)
	gzName := base + ".jsonl.gz"
	gzPath := filepath.Join(dir, gzName)
	gzFile, err := os.OpenFile(gzPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ArchiveReport{}, err
	}
	gz := gzip.NewWriter(gzFile)
	enc := json.NewEncoder(gz)
	for _, ev := range old {
		if err := enc.Encode(ev); err != nil {
			_ = gz.Close()
			_ = gzFile.Close()
			return ArchiveReport{}, err
		}
	}
	if err := gz.Close(); err != nil {
		_ = gzFile.Close()
		return ArchiveReport{}, err
	}
	if err := gzFile.Close(); err != nil {
		return ArchiveReport{}, err
	}
	headHash := old[len(old)-1].Hash
	manifest := ArchiveManifest{
		ArchiveFile:   gzName,
		FirstSeq:      old[0].Seq,
		LastSeq:       old[len(old)-1].Seq,
		FirstTime:     old[0].Time,
		LastTime:      old[len(old)-1].Time,
		EventCount:    len(old),
		HeadHash:      headHash,
		ChainValid:    true,
		RetentionDays: l.retentionDays,
		CreatedAt:     time.Now().UTC(),
	}
	manifestName := base + ".manifest.json"
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, manifestName), append(manifestBytes, '\n'), 0o600); err != nil {
		return ArchiveReport{}, err
	}
	// Checkpoint anchors the pruned live log. It reuses the archived
	// head's sequence number: within the live file it is unique (the
	// kept events start at head+1), and it marks exactly where the
	// archived segment ends.
	cp := Event{
		Seq:      old[len(old)-1].Seq,
		Time:     time.Now().UTC(),
		Actor:    actor,
		Kind:     "admin",
		Action:   CheckpointAction,
		Decision: "allow",
		Detail: map[string]string{
			"archive_file":       gzName,
			"archived_first_seq": strconv.FormatInt(old[0].Seq, 10),
			"archived_last_seq":  strconv.FormatInt(old[len(old)-1].Seq, 10),
			"archived_events":    strconv.Itoa(len(old)),
			"archived_head_hash": headHash,
			"retention_days":     strconv.Itoa(l.retentionDays),
		},
		PrevHash: "genesis",
	}
	cp.Hash = hashEvent(cp)
	// Rewrite the live file atomically: checkpoint first, then the
	// kept events byte-identical to before.
	tmpPath := l.path + ".tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return ArchiveReport{}, err
	}
	writeLine := func(ev Event) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		_, err = tmp.Write(append(b, '\n'))
		return err
	}
	if err := writeLine(cp); err != nil {
		_ = tmp.Close()
		return ArchiveReport{}, err
	}
	for _, ev := range keep {
		if err := writeLine(ev); err != nil {
			_ = tmp.Close()
			return ArchiveReport{}, err
		}
	}
	if err := tmp.Close(); err != nil {
		return ArchiveReport{}, err
	}
	if err := os.Rename(tmpPath, l.path); err != nil {
		return ArchiveReport{}, err
	}
	l.events = append([]Event{cp}, keep...)
	if len(keep) > 0 {
		l.seq = keep[len(keep)-1].Seq
	} else {
		l.seq = cp.Seq
	}
	l.lastHash = chainHeadHash(l.events)
	return ArchiveReport{
		ArchiveFile:   gzName,
		ManifestFile:  manifestName,
		Archived:      len(old),
		FirstSeq:      old[0].Seq,
		LastSeq:       old[len(old)-1].Seq,
		HeadHash:      headHash,
		ChainValid:    true,
		RetentionDays: l.retentionDays,
		LiveEvents:    len(l.events),
	}, nil
}
