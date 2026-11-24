package main

// Backup CLI (backlog F27 phase 3 B1): `agentgate backup <command>`
// runs without a server and without the master key, so restore works
// when the data directory is empty. All commands use environment
// configuration (AGENTGATE_BACKUP_S3_*); the server's stored console
// config is not read here — a restore must not depend on the state
// it is about to replace. The CLI never appends to the audit log
// (it must not write a log another process owns) and never prints a
// secret; the access key is shown masked.
//
// Commands:
//
//	upload          run one backup cycle now (state + segments + prune)
//	restore <dir>   fetch the latest state + all segments into an
//	                empty dir and verify the chain there; refuses the
//	                live data directory (stop the server first)
//	list            list remote objects
//	verify <dir>    verify the full audit history in a local dir
//	status          show the effective configuration (masked)

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/backup"
)

func backupUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentgate backup <upload|restore <dir>|list|verify <dir>|status>")
	fmt.Fprintln(os.Stderr, "  environment: AGENTGATE_DATA (default ./data), AGENTGATE_BACKUP_S3_ENDPOINT,")
	fmt.Fprintln(os.Stderr, "  AGENTGATE_BACKUP_S3_BUCKET, AGENTGATE_BACKUP_S3_ACCESS_KEY, AGENTGATE_BACKUP_S3_SECRET_KEY")
	fmt.Fprintln(os.Stderr, "  (see docs/backup.md)")
}

// newBackupCLI builds the backup service over environment config
// and a disk read of the live state file.
func newBackupCLI() (*backup.Service, error) {
	dataDir := envOr("AGENTGATE_DATA", "./data")
	if _, err := backup.EnvConfig(); err != nil {
		return nil, err
	}
	svc := backup.NewService(dataDir,
		func() (backup.Config, error) { return backup.EnvConfig() },
		func() ([]byte, error) { return os.ReadFile(filepath.Join(dataDir, "state.json")) },
		nil, nil,
		func(format string, args ...any) { fmt.Printf(format+"\n", args...) })
	return svc, nil
}

func runBackupCLI(args []string) int {
	if len(args) == 0 {
		backupUsage()
		return 2
	}
	svc, err := newBackupCLI()
	if err != nil {
		if errors.Is(err, backup.ErrNotConfigured) {
			fmt.Fprintln(os.Stderr, "backup: not configured — set AGENTGATE_BACKUP_S3_ENDPOINT, AGENTGATE_BACKUP_S3_BUCKET, AGENTGATE_BACKUP_S3_ACCESS_KEY and AGENTGATE_BACKUP_S3_SECRET_KEY (see docs/backup.md)")
		} else {
			fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		}
		return 1
	}
	ctx := context.Background()
	switch args[0] {
	case "upload":
		rep, err := svc.RunOnce(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "backup: upload failed (%s): %v\n", backup.ErrorClass(err), err)
			return 1
		}
		fmt.Printf("backup: state %s uploaded, %d audit segment(s), %d pruned\n",
			rep.StateKey, len(rep.AuditUploaded), len(rep.Pruned))
		return 0
	case "restore":
		if len(args) < 2 {
			backupUsage()
			return 2
		}
		rep, err := svc.Restore(ctx, args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "backup: restore failed: %v\n", err)
			if rep.Segments > 0 || rep.TotalEvents > 0 {
				fmt.Fprintf(os.Stderr, "backup: partial restore: %d segment(s), %d event(s), chain valid: %v\n",
					rep.Segments, rep.TotalEvents, rep.ChainValid)
			}
			return 1
		}
		fmt.Printf("backup: restored state %s + %d audit segment(s) (%d events) to %s; chain valid: %v; live log anchored: %v\n",
			rep.StateKey, rep.Segments, rep.TotalEvents, rep.DestDir, rep.ChainValid, rep.AnchorWritten)
		return 0
	case "list":
		objs, err := svc.ListRemote(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "backup: list failed: %v\n", err)
			return 1
		}
		for _, o := range objs {
			fmt.Printf("%s\t%d\t%s\n", o.Key, o.Size, o.LastModified.Format("2006-01-02T15:04:05Z"))
		}
		return 0
	case "verify":
		if len(args) < 2 {
			backupUsage()
			return 2
		}
		rep, err := audit.VerifyFullHistory(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "backup: verify failed: %v\n", err)
			return 1
		}
		fmt.Printf("backup: %d segment(s), %d segment event(s), %d live event(s), %d total; chain valid: %v\n",
			rep.Segments, rep.SegmentEvents, rep.LiveEvents, rep.TotalEvents, rep.ChainValid)
		if !rep.ChainValid {
			return 1
		}
		return 0
	case "status":
		cfg, err := svc.EffectiveConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "backup: %v\n", err)
			return 1
		}
		fmt.Printf("backup: endpoint %s\n  bucket %s (region %s, source %s)\n  access key %s (secret set: %v)\n  state every %s, retain %s; audit retain %s\n",
			cfg.Endpoint, cfg.Bucket, cfg.Region, cfg.Source, cfg.MaskedAccessKey(),
			cfg.SecretKey != "", cfg.StateInterval, cfg.StateRetention, cfg.AuditRetention)
		return 0
	default:
		backupUsage()
		return 2
	}
}
