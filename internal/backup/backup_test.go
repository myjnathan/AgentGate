package backup

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
)

func testConfig(tsURL string) Config {
	return Config{Endpoint: tsURL, Bucket: "bkt", Region: "us-east-1",
		AccessKey: "ak", SecretKey: "sk",
		StateInterval: time.Hour, StateRetention: time.Hour, AuditRetention: time.Hour,
		Source: "test"}.WithDefaults()
}

// testDataDir builds a data dir with an audit log of 4 events, the
// first 2 archived into a segment, like the production archive flow.
func testDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	l, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	l.SetRetentionDays(180)
	for _, ev := range []audit.Event{
		{Actor: "alice", Kind: "model_call", Action: "chat_completions"},
		{Actor: "bob", Kind: "model_call", Action: "chat_completions"},
		{Actor: "alice", Kind: "violation", Action: "model_call_blocked", Decision: "block"},
		{Actor: "alice", Kind: "admin", Action: "channel_created"},
	} {
		l.Append(ev)
	}
	// Archive the first two events (cutoff between event 2 and 3).
	evs := l.All()
	if _, err := l.Archive(evs[1].Time.Add(time.Nanosecond), "admin"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestServiceRunOnceAndRestore(t *testing.T) {
	srv := newFakeS3()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := testDataDir(t)
	var audited []audit.Event
	svc := NewService(dir,
		func() (Config, error) { return testConfig(ts.URL), nil },
		func() ([]byte, error) { return []byte(`{"users":{"admin":{}}}`), nil },
		func(ev audit.Event) { audited = append(audited, ev) },
		nil, t.Logf)
	ctx := context.Background()

	rep, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.StateUploaded || rep.StateKey == "" || len(rep.AuditUploaded) != 1 {
		t.Fatalf("first run report = %+v, want state + 1 segment", rep)
	}
	if len(audited) != 1 || audited[0].Action != "backup_uploaded" {
		t.Fatalf("audited = %+v, want one backup_uploaded", audited)
	}
	for k, v := range audited[0].Detail {
		if strings.Contains(v, "sk") || strings.Contains(k, "secret") {
			t.Fatalf("audit detail leaks secret material: %q=%q", k, v)
		}
	}
	// Second run: segments already present, only a new state snapshot.
	rep2, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.StateUploaded || len(rep2.AuditUploaded) != 0 {
		t.Fatalf("second run report = %+v, want state only", rep2)
	}

	// Restore into a fresh dir: state + segment come back, the chain
	// verifies, and the live log is anchored at the segment head.
	dest := filepath.Join(t.TempDir(), "restored")
	rr, err := svc.Restore(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.ChainValid || rr.Segments != 1 || rr.TotalEvents != 2 || !rr.AnchorWritten {
		t.Fatalf("restore report = %+v, want valid, 1 segment, 2 events, anchor", rr)
	}
	if _, err := os.Stat(filepath.Join(dest, "state.json")); err != nil {
		t.Fatalf("restored state.json missing: %v", err)
	}
	// The restored server continues the chain: a new event links
	// from the archived head with the next sequence number.
	l, err := audit.Open(filepath.Join(dest, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ev := l.Append(audit.Event{Actor: "alice", Kind: "model_call", Action: "chat_completions"})
	if ev.Seq != 3 || ev.PrevHash == "genesis" {
		t.Fatalf("post-restore event = seq %d prev %q, want seq 3 linked to segment head", ev.Seq, ev.PrevHash)
	}
	if !l.Verify() {
		t.Fatal("post-restore live log does not verify")
	}
	// Restore refuses the live dir and a non-empty destination.
	if _, err := svc.Restore(ctx, dir); err == nil {
		t.Fatal("restore into live dir must fail")
	}
	if _, err := svc.Restore(ctx, dest); err == nil {
		t.Fatal("restore into non-empty dir must fail")
	}
}

func TestServicePrune(t *testing.T) {
	srv := newFakeS3()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := testDataDir(t)
	svc := NewService(dir,
		func() (Config, error) { return testConfig(ts.URL), nil },
		func() ([]byte, error) { return []byte(`{}`), nil },
		nil, nil, nil)
	ctx := context.Background()
	if _, err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// An old state snapshot and an old audit object, both past the
	// 1h test retention, must be pruned; the newest state snapshot
	// is never pruned even when old.
	c := NewClient(testConfig(ts.URL))
	oldState := "state/state-2026-01-01T00-00-00Z.json"
	if err := c.PutObject(ctx, oldState, []byte("{}"), "application/json"); err != nil {
		t.Fatal(err)
	}
	srv.setOld(oldState, time.Now().Add(-2*time.Hour))
	oldAudit := "audit/audit-archive-9-9.jsonl.gz"
	if err := c.PutObject(ctx, oldAudit, []byte("gz"), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	srv.setOld(oldAudit, time.Now().Add(-2*time.Hour))
	rep, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, k := range rep.Pruned {
		found[k] = true
	}
	if !found[oldState] || !found[oldAudit] {
		t.Fatalf("pruned = %v, want both old objects", rep.Pruned)
	}
	if found[rep.StateKey] {
		t.Fatalf("newest state snapshot %s was pruned", rep.StateKey)
	}
}

func TestServiceNotConfigured(t *testing.T) {
	svc := NewService(t.TempDir(),
		func() (Config, error) { return Config{}, ErrNotConfigured },
		func() ([]byte, error) { return []byte("{}"), nil },
		nil, nil, nil)
	rep, err := svc.RunOnce(context.Background())
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("run without config = %v, want ErrNotConfigured", err)
	}
	if rep.Error != "not_configured" {
		t.Fatalf("report error = %q, want not_configured", rep.Error)
	}
}

func TestServiceRunPeriodic(t *testing.T) {
	srv := newFakeS3()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := testConfig(ts.URL)
	cfg.StateInterval = 10 * time.Millisecond
	runs := 0
	svc := NewService(t.TempDir(),
		func() (Config, error) { return cfg, nil },
		func() ([]byte, error) { return []byte("{}"), nil },
		nil,
		func(Report) { runs++ },
		nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); svc.RunPeriodic(ctx) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPeriodic did not stop on cancel")
	}
	if runs == 0 {
		t.Fatal("periodic runner ran zero cycles")
	}
}

func TestServiceRunOnceCreatesBucket(t *testing.T) {
	// Registration 13: a backup cycle against a fresh endpoint
	// (bucket never created) must create the bucket itself and
	// succeed — the 2026-11-24 drill failed here before the fix.
	srv := newFakeS3()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := testDataDir(t)
	svc := NewService(dir,
		func() (Config, error) { return testConfig(ts.URL), nil },
		func() ([]byte, error) { return []byte(`{}`), nil },
		nil, nil, nil)
	rep, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce on a fresh endpoint = %v", err)
	}
	if !rep.StateUploaded || len(rep.AuditUploaded) != 1 {
		t.Fatalf("report = %+v, want state + 1 segment", rep)
	}
	if !srv.buckets["bkt"] {
		t.Fatal("RunOnce did not create the bucket")
	}
}
