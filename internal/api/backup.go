package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/backup"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// Object-storage backup (backlog F27 phase 3 B1): status, console
// configuration and manual runs. The S3 secret key is AES-GCM
// encrypted with the master key before it is stored (like a webhook
// secret) and is never returned by any endpoint — only the masked
// form is displayed. All three endpoints are admin-only: backup
// configuration and runs are operator territory, and the status
// shows infrastructure config.

// backupStatusResponse is the GET /api/backup/status body.
type backupStatusResponse struct {
	Configured         bool                    `json:"configured"`
	Source             string                  `json:"source,omitempty"` // stored | env
	Endpoint           string                  `json:"endpoint,omitempty"`
	Bucket             string                  `json:"bucket,omitempty"`
	Region             string                  `json:"region,omitempty"`
	AccessKeyMasked    string                  `json:"access_key_masked,omitempty"`
	HasSecret          bool                    `json:"has_secret"`
	StateIntervalHours int                     `json:"state_interval_hours,omitempty"`
	StateRetentionDays int                     `json:"state_retention_days,omitempty"`
	AuditRetentionDays int                     `json:"audit_retention_days,omitempty"`
	LastRun            *store.BackupRunSummary `json:"last_run,omitempty"`
}

func (a *API) backupStatusView() backupStatusResponse {
	var lastRun *store.BackupRunSummary
	a.st.Read(func(s *store.State) {
		if s.Backup != nil {
			lastRun = s.Backup.LastRun
		}
	})
	resp := backupStatusResponse{LastRun: lastRun}
	if a.backupSvc == nil {
		return resp
	}
	cfg, err := a.backupSvc.EffectiveConfig()
	if err != nil {
		return resp
	}
	resp.Configured = true
	resp.Source = cfg.Source
	resp.Endpoint = cfg.Endpoint
	resp.Bucket = cfg.Bucket
	resp.Region = cfg.Region
	resp.AccessKeyMasked = cfg.MaskedAccessKey()
	resp.HasSecret = cfg.SecretKey != ""
	resp.StateIntervalHours = int(cfg.StateInterval / time.Hour)
	resp.StateRetentionDays = int(cfg.StateRetention / (24 * time.Hour))
	resp.AuditRetentionDays = int(cfg.AuditRetention / (24 * time.Hour))
	return resp
}

// GET /api/backup/status — effective config (masked) + last run.
func (a *API) backupStatus(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, a.backupStatusView())
}

// POST /api/backup/config — store the S3-compatible endpoint,
// bucket and credentials, encrypted. The secret is never echoed.
func (a *API) setBackupConfig(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Endpoint           string `json:"endpoint"`
		Bucket             string `json:"bucket"`
		Region             string `json:"region"`
		AccessKey          string `json:"access_key"`
		SecretKey          string `json:"secret_key"`
		StateIntervalHours int    `json:"state_interval_hours"`
		StateRetentionDays int    `json:"state_retention_days"`
		AuditRetentionDays int    `json:"audit_retention_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.Endpoint == "" || in.Bucket == "" || in.AccessKey == "" || in.SecretKey == "" {
		writeErr(w, http.StatusBadRequest, "endpoint, bucket, access_key and secret_key are required")
		return
	}
	cfg := backup.Config{
		Endpoint:  in.Endpoint,
		Bucket:    in.Bucket,
		Region:    in.Region,
		AccessKey: in.AccessKey,
		SecretKey: in.SecretKey,
		Source:    "stored",
	}.WithDefaults()
	if in.StateIntervalHours > 0 {
		cfg.StateInterval = time.Duration(in.StateIntervalHours) * time.Hour
	}
	if in.StateRetentionDays > 0 {
		cfg.StateRetention = time.Duration(in.StateRetentionDays) * 24 * time.Hour
	}
	if in.AuditRetentionDays > 0 {
		cfg.AuditRetention = time.Duration(in.AuditRetentionDays) * 24 * time.Hour
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enc, err := a.box.Encrypt(in.SecretKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encrypt failed")
		return
	}
	stored := &store.BackupConfig{
		Endpoint:           cfg.Endpoint,
		Bucket:             cfg.Bucket,
		Region:             cfg.Region,
		AccessKey:          cfg.AccessKey,
		SecretEnc:          enc,
		SecretMasked:       secretbox.Mask(in.SecretKey),
		StateIntervalHours: int(cfg.StateInterval / time.Hour),
		StateRetentionDays: int(cfg.StateRetention / (24 * time.Hour)),
		AuditRetentionDays: int(cfg.AuditRetention / (24 * time.Hour)),
	}
	a.st.WithLock(func(s *store.State) {
		if s.Backup != nil {
			stored.LastRun = s.Backup.LastRun
		}
		s.Backup = stored
	})
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save failed")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin",
		Action: "backup_configured", Decision: "allow",
		Detail: map[string]string{"bucket": cfg.Bucket, "region": cfg.Region}})
	writeJSON(w, http.StatusOK, a.backupStatusView())
}

// POST /api/backup/run — run one backup cycle now (state snapshot +
// pending audit segments + prune). The cycle itself appends its own
// audit event; this handler only reports the outcome.
func (a *API) runBackup(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.backupSvc == nil {
		writeErr(w, http.StatusServiceUnavailable, "backup_not_configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	rep, err := a.backupSvc.RunOnce(ctx)
	if err != nil {
		if errors.Is(err, backup.ErrNotConfigured) {
			writeErr(w, http.StatusServiceUnavailable, "backup_not_configured")
			return
		}
		writeErr(w, http.StatusBadGateway, backup.ErrorClass(err))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
