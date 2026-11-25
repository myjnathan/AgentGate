package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
)

// Report is the outcome of one backup cycle. Error holds an error
// class only (see ErrorClass), never a secret, URL or bucket detail.
type Report struct {
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	StateUploaded bool      `json:"state_uploaded"`
	StateKey      string    `json:"state_key,omitempty"`
	AuditUploaded []string  `json:"audit_uploaded,omitempty"`
	AuditSkipped  int       `json:"audit_skipped"`
	Pruned        []string  `json:"pruned,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// RestoreReport is the outcome of a restore: what was fetched and
// whether the full history chain verifies. ChainValid false is a
// failed restore — the caller must not start a server on it.
type RestoreReport struct {
	StateKey      string `json:"state_key"`
	Segments      int    `json:"segments"`
	SegmentEvents int    `json:"segment_events"`
	LiveEvents    int    `json:"live_events"`
	TotalEvents   int    `json:"total_events"`
	ChainValid    bool   `json:"chain_valid"`
	// AnchorWritten says the restored live log starts with a
	// checkpoint anchoring it at the last segment's head, so new
	// events continue the chain (false when there were no segments
	// to continue from).
	AnchorWritten bool   `json:"anchor_written"`
	DestDir       string `json:"dest_dir"`
}

// ErrorClass maps an error to the audit-safe class recorded in
// reports and audit events (no secrets, no URLs).
func ErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrNotConfigured) {
		return "not_configured"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "s3_timeout"
	}
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case 401, 403:
			return "s3_auth"
		case 404:
			return "s3_not_found"
		default:
			return "s3_error"
		}
	}
	if errors.Is(err, ErrNotFound) {
		return "s3_not_found"
	}
	return "s3_error"
}

// Service runs backup cycles against one data directory. It is safe
// for concurrent use: runs are serialized, so a manual run and the
// periodic tick can never upload the same snapshot twice.
type Service struct {
	dataDir string
	// config returns the effective configuration on every call, so a
	// console change takes effect without a restart.
	config func() (Config, error)
	// stateSnapshot returns the live state bytes (the store's own
	// serialization); the CLI passes a disk reader instead.
	stateSnapshot func() ([]byte, error)
	// auditFn receives backup audit events (nil in the CLI, which
	// must not append to a log another process owns).
	auditFn func(audit.Event)
	// onRun is called after every cycle with its report (nil when
	// unused); the server persists the last run for the status page.
	onRun func(Report)
	// logf is the operator log (nil = silent).
	logf func(string, ...any)

	mu sync.Mutex
}

// NewService builds a backup service. Any of auditFn, onRun and
// logf may be nil.
func NewService(dataDir string, config func() (Config, error), stateSnapshot func() ([]byte, error),
	auditFn func(audit.Event), onRun func(Report), logf func(string, ...any)) *Service {
	return &Service{
		dataDir:       dataDir,
		config:        config,
		stateSnapshot: stateSnapshot,
		auditFn:       auditFn,
		onRun:         onRun,
		logf:          logf,
	}
}

// EffectiveConfig returns the current effective configuration.
func (s *Service) EffectiveConfig() (Config, error) { return s.config() }

func (s *Service) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

func (s *Service) appendAudit(ev audit.Event) {
	if s.auditFn != nil {
		s.auditFn(ev)
	}
}

// RunOnce uploads one state snapshot and every not-yet-uploaded
// audit archive segment, then prunes remote objects past retention.
// The state snapshot is uploaded on every cycle (hourly by default),
// even when unchanged — a snapshot exists for every hour, which is
// what the runbook's "state snapshot hourly" baseline promises.
func (s *Service) RunOnce(ctx context.Context) (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := Report{StartedAt: time.Now().UTC()}
	fail := func(err error) (Report, error) {
		rep.FinishedAt = time.Now().UTC()
		rep.Error = ErrorClass(err)
		s.appendAudit(audit.Event{Actor: "system", Kind: "admin", Action: "backup_failed",
			Decision: "allow", Detail: map[string]string{"error": rep.Error}})
		if s.onRun != nil {
			s.onRun(rep)
		}
		return rep, err
	}
	cfg, err := s.config()
	if err != nil {
		return fail(err)
	}
	client := NewClient(cfg)
	if created, err := client.EnsureBucket(ctx); err != nil {
		return fail(err)
	} else if created {
		s.log("backup: created object storage bucket (first upload)")
	}
	if err := s.uploadState(ctx, client, &rep); err != nil {
		return fail(err)
	}
	if err := s.uploadSegments(ctx, client, &rep); err != nil {
		return fail(err)
	}
	if err := s.prune(ctx, client, cfg, &rep); err != nil {
		return fail(err)
	}
	rep.FinishedAt = time.Now().UTC()
	s.appendAudit(audit.Event{Actor: "system", Kind: "admin", Action: "backup_uploaded",
		Decision: "allow", Detail: map[string]string{
			"state_key":      rep.StateKey,
			"audit_uploaded": fmt.Sprintf("%d", len(rep.AuditUploaded)),
			"audit_skipped":  fmt.Sprintf("%d", rep.AuditSkipped),
			"pruned":         fmt.Sprintf("%d", len(rep.Pruned)),
		}})
	if s.onRun != nil {
		s.onRun(rep)
	}
	s.log("backup: uploaded state %s, %d audit segment(s), pruned %d object(s)",
		rep.StateKey, len(rep.AuditUploaded), len(rep.Pruned))
	return rep, nil
}

// uploadState snapshots the live state and uploads it under a
// timestamped key. A missing state file (fresh install, nothing
// saved yet) is skipped, not an error.
func (s *Service) uploadState(ctx context.Context, client *Client, rep *Report) error {
	b, err := s.stateSnapshot()
	if err != nil {
		if os.IsNotExist(err) {
			s.log("backup: no state.json yet, skipping state snapshot")
			return nil
		}
		return fmt.Errorf("read state: %w", err)
	}
	key := StatePrefix + "state-" + time.Now().UTC().Format("2006-01-02T15-04-05Z") + ".json"
	if err := client.PutObject(ctx, key, b, "application/json"); err != nil {
		return err
	}
	rep.StateUploaded = true
	rep.StateKey = key
	return nil
}

// uploadSegments uploads every local audit archive segment (plus its
// manifest) whose remote key is absent. A .gz without a local
// manifest is skipped — it could not be verified on restore — and
// counted, not fatal.
func (s *Service) uploadSegments(ctx context.Context, client *Client, rep *Report) error {
	remote, err := client.ListObjects(ctx, AuditPrefix)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, o := range remote {
		have[o.Key] = true
	}
	names, err := filepath.Glob(filepath.Join(s.dataDir, "audit-archive-*.jsonl.gz"))
	if err != nil {
		return err
	}
	for _, path := range names {
		base := filepath.Base(path)
		manifest := strings.TrimSuffix(base, ".jsonl.gz") + ".manifest.json"
		if _, err := os.Stat(filepath.Join(s.dataDir, manifest)); err != nil {
			s.log("backup: skipping segment %s (no local manifest)", base)
			rep.AuditSkipped++
			continue
		}
		for _, name := range []string{base, manifest} {
			key := AuditPrefix + name
			if have[key] {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.dataDir, name))
			if err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
			ct := "application/gzip"
			if strings.HasSuffix(name, ".json") {
				ct = "application/json"
			}
			if err := client.PutObject(ctx, key, b, ct); err != nil {
				return err
			}
			if strings.HasSuffix(name, ".jsonl.gz") {
				rep.AuditUploaded = append(rep.AuditUploaded, name)
			}
		}
	}
	return nil
}

// prune deletes remote state snapshots older than StateRetention
// (never the newest one, even when old) and remote audit objects
// older than AuditRetention.
func (s *Service) prune(ctx context.Context, client *Client, cfg Config, rep *Report) error {
	now := time.Now()
	states, err := client.ListObjects(ctx, StatePrefix)
	if err != nil {
		return err
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Key < states[j].Key })
	for i, o := range states {
		newest := i == len(states)-1
		if !newest && now.Sub(o.LastModified) > cfg.StateRetention {
			if err := client.DeleteObject(ctx, o.Key); err != nil {
				return err
			}
			rep.Pruned = append(rep.Pruned, o.Key)
		}
	}
	audits, err := client.ListObjects(ctx, AuditPrefix)
	if err != nil {
		return err
	}
	for _, o := range audits {
		if now.Sub(o.LastModified) > cfg.AuditRetention {
			if err := client.DeleteObject(ctx, o.Key); err != nil {
				return err
			}
			rep.Pruned = append(rep.Pruned, o.Key)
		}
	}
	return nil
}

// ListRemote lists every object in the bucket (state + audit).
func (s *Service) ListRemote(ctx context.Context) ([]Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.config()
	if err != nil {
		return nil, err
	}
	client := NewClient(cfg)
	var out []Object
	for _, prefix := range []string{StatePrefix, AuditPrefix} {
		objs, err := client.ListObjects(ctx, prefix)
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Restore fetches the latest state snapshot and all audit segments
// into destDir, then verifies the full history chain there. The
// destination must be empty (no state.json yet) and must not be the
// live data directory: restore never overwrites live data. When
// segments exist, the restored live log starts with a checkpoint
// anchoring it at the last segment's head, so new events continue
// the chain (see audit.AnchorCheckpoint). A chain that does not
// verify is a failed restore — the files stay on disk for
// inspection, but the caller must not start a server on them.
func (s *Service) Restore(ctx context.Context, destDir string) (RestoreReport, error) {
	var rep RestoreReport
	cfg, err := s.config()
	if err != nil {
		return rep, err
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return rep, err
	}
	absLive, err := filepath.Abs(s.dataDir)
	if err != nil {
		return rep, err
	}
	if absDest == absLive {
		return rep, errors.New("restore refuses the live data directory: use an empty directory")
	}
	if _, err := os.Stat(filepath.Join(destDir, "state.json")); err == nil {
		return rep, errors.New("restore refuses a non-empty destination: state.json already exists")
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return rep, err
	}
	client := NewClient(cfg)
	states, err := client.ListObjects(ctx, StatePrefix)
	if err != nil {
		return rep, err
	}
	if len(states) == 0 {
		return rep, errors.New("no state snapshot in bucket")
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Key < states[j].Key })
	latest := states[len(states)-1]
	stateBytes, err := client.GetObject(ctx, latest.Key)
	if err != nil {
		return rep, err
	}
	if !json.Valid(stateBytes) {
		return rep, fmt.Errorf("state snapshot %s is not valid JSON", latest.Key)
	}
	if err := writeFileAtomic(filepath.Join(destDir, "state.json"), stateBytes); err != nil {
		return rep, err
	}
	rep.StateKey = latest.Key
	audits, err := client.ListObjects(ctx, AuditPrefix)
	if err != nil {
		return rep, err
	}
	for _, o := range audits {
		b, err := client.GetObject(ctx, o.Key)
		if err != nil {
			return rep, err
		}
		name := strings.TrimPrefix(o.Key, AuditPrefix)
		if err := writeFileAtomic(filepath.Join(destDir, name), b); err != nil {
			return rep, err
		}
	}
	// Anchor the restored live log at the last segment's head, so
	// new events continue the chain instead of restarting it at
	// genesis (see audit.AnchorCheckpoint). With no segments there
	// is no history to continue: the live log starts fresh.
	if len(audits) > 0 {
		head, err := lastSegmentHead(destDir)
		if err != nil {
			return rep, err
		}
		anchor := audit.AnchorCheckpoint(head.HeadHash, head.LastSeq, head.ArchiveFile, "system")
		b, err := json.Marshal(anchor)
		if err != nil {
			return rep, err
		}
		if err := writeFileAtomic(filepath.Join(destDir, "audit.jsonl"), append(b, '\n')); err != nil {
			return rep, err
		}
		rep.AnchorWritten = true
	}
	hist, err := audit.VerifyFullHistory(destDir)
	if err != nil {
		return rep, fmt.Errorf("verify restored history: %w", err)
	}
	rep.Segments = hist.Segments
	rep.SegmentEvents = hist.SegmentEvents
	rep.LiveEvents = hist.LiveEvents
	rep.TotalEvents = hist.TotalEvents
	rep.ChainValid = hist.ChainValid
	rep.DestDir = destDir
	if !hist.ChainValid {
		return rep, fmt.Errorf("restored audit chain does not verify (%d segments, %d events)", hist.Segments, hist.TotalEvents)
	}
	s.log("backup: restored state %s + %d audit segment(s) to %s, chain valid (%d events)",
		rep.StateKey, rep.Segments, destDir, rep.TotalEvents)
	return rep, nil
}

// lastSegmentHead returns the manifest of the segment with the
// highest last sequence in destDir — the head the restored live log
// anchors at.
func lastSegmentHead(destDir string) (audit.ArchiveManifest, error) {
	var head audit.ArchiveManifest
	names, err := filepath.Glob(filepath.Join(destDir, "audit-archive-*.manifest.json"))
	if err != nil {
		return head, err
	}
	if len(names) == 0 {
		return head, errors.New("audit segments downloaded but no manifest found; refusing to anchor")
	}
	for _, name := range names {
		b, err := os.ReadFile(name)
		if err != nil {
			return head, err
		}
		var man audit.ArchiveManifest
		if err := json.Unmarshal(b, &man); err != nil {
			return head, fmt.Errorf("manifest %s: %w", filepath.Base(name), err)
		}
		if man.LastSeq > head.LastSeq {
			head = man
		}
	}
	return head, nil
}

// RunPeriodic runs a cycle now and then every StateInterval until
// ctx ends. An unconfigured cycle is skipped quietly (the startup
// log already says backups are disabled); a failed cycle is logged
// and audited, never fatal — the next tick retries.
func (s *Service) RunPeriodic(ctx context.Context) {
	for {
		cfg, err := s.config()
		interval := DefaultStateInterval
		if err == nil {
			interval = cfg.StateInterval
			if _, runErr := s.RunOnce(ctx); runErr != nil {
				s.log("backup: cycle failed: %v", runErr)
			}
		} else if !errors.Is(err, ErrNotConfigured) {
			s.log("backup: config error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// writeFileAtomic writes b to path (0600) via a temp file + rename,
// so a reader never sees a torn file.
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
