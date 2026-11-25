// Package backup implements the F27 phase-3 B1 object-storage backup:
// periodic state snapshots and audit archive segments uploaded to an
// S3-compatible bucket, plus retrieval with full-history chain
// verification.
//
// The bucket is self-hosted on the same machine or intranet (never a
// cloud bucket, per Nathan's 2026-11-24 decision); the runbook
// (docs/backup.md) recommends Garage or rclone serve s3 for a new
// self-hosted deployment. MinIO's community server is no longer
// distributed (dl.min.io returns 410 "archived and no longer
// maintained", verified 2026-11-24), so it is not the default
// recommendation — any S3-compatible endpoint works, the client only
// speaks the S3 API.
//
// The S3 client is hand-rolled on the standard library (SigV4
// signing, path-style addressing): only PUT/GET/HEAD/LIST/DELETE are
// needed, and a minio-go dependency would break the single-binary
// zero-dependency positioning for a handful of calls. The client
// also speaks bucket HEAD/PUT (EnsureBucket) so a fresh endpoint
// needs no hand-created bucket. Signing is
// verified against the AWS-documented SigV4 example vector in the
// tests and against a real S3 server (rclone serve s3) in the drill.
package backup

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/secretbox"
)

// ErrNotConfigured is returned when no object-storage backup is
// configured (no stored config, no environment config).
var ErrNotConfigured = errors.New("backup: object storage not configured")

// Defaults (registration 11 baseline, Nathan 2026-11-24): state
// snapshot hourly, kept 7 days; audit archive segments kept 90 days;
// drills quarterly (a schedule, not a code constant — see the
// runbook).
const (
	DefaultRegion         = "us-east-1"
	DefaultStateInterval  = time.Hour
	DefaultStateRetention = 7 * 24 * time.Hour
	DefaultAuditRetention = 90 * 24 * time.Hour
)

// Object key prefixes in the bucket. Keys sort lexicographically in
// chronological order, so "latest" is the last key.
const (
	StatePrefix = "state/"
	AuditPrefix = "audit/"
)

// Config is the effective backup configuration: the secret key lives
// in memory only, never in a log, an audit event or an API response.
// Persisted form is store.BackupConfig (secret AES-GCM-encrypted with
// the master key); the CLI uses environment config only.
type Config struct {
	Endpoint       string
	Bucket         string
	Region         string
	AccessKey      string
	SecretKey      string
	StateInterval  time.Duration
	StateRetention time.Duration
	AuditRetention time.Duration
	// Source says where the config came from ("stored" = console
	// config in state.json, "env" = environment variables), for the
	// status display.
	Source string
}

// WithDefaults fills zero fields with the documented defaults.
func (c Config) WithDefaults() Config {
	if c.Region == "" {
		c.Region = DefaultRegion
	}
	if c.StateInterval <= 0 {
		c.StateInterval = DefaultStateInterval
	}
	if c.StateRetention <= 0 {
		c.StateRetention = DefaultStateRetention
	}
	if c.AuditRetention <= 0 {
		c.AuditRetention = DefaultAuditRetention
	}
	return c
}

// MaskedAccessKey returns the access key in display-safe form
// ("****" + last 4 chars); the secret key is never displayed.
func (c Config) MaskedAccessKey() string { return secretbox.Mask(c.AccessKey) }

// Validate checks a config before it is used. The endpoint must be a
// bare http(s) origin (no path, query or userinfo — credentials in a
// URL would leak into logs); the bucket must be a plausible S3 name.
func (c Config) Validate() error {
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint %q: %w", c.Endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint %q must be http(s)", c.Endpoint)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q has no host", c.Endpoint)
	}
	if u.User != nil {
		return fmt.Errorf("endpoint %q must not carry credentials", c.Endpoint)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("endpoint %q must not carry a path", c.Endpoint)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("endpoint %q must not carry a query or fragment", c.Endpoint)
	}
	if err := ValidateBucket(c.Bucket); err != nil {
		return err
	}
	if c.AccessKey == "" {
		return errors.New("access key is required")
	}
	if c.SecretKey == "" {
		return errors.New("secret key is required")
	}
	if c.StateInterval <= 0 {
		return errors.New("state interval must be positive")
	}
	if c.StateRetention <= 0 {
		return errors.New("state retention must be positive")
	}
	if c.AuditRetention <= 0 {
		return errors.New("audit retention must be positive")
	}
	return nil
}

// ValidateBucket checks the simplified S3 bucket rules the client
// relies on (lowercase, digits, dots and hyphens, 3-63 chars).
func ValidateBucket(bucket string) error {
	if len(bucket) < 3 || len(bucket) > 63 {
		return fmt.Errorf("bucket %q must be 3-63 characters", bucket)
	}
	for i, r := range bucket {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-'
		if !ok {
			return fmt.Errorf("bucket %q has an illegal character %q", bucket, r)
		}
		if (i == 0 || i == len(bucket)-1) && (r == '.' || r == '-') {
			return fmt.Errorf("bucket %q must start and end with a letter or digit", bucket)
		}
	}
	if strings.Contains(bucket, "..") {
		return fmt.Errorf("bucket %q must not contain \"..\"", bucket)
	}
	return nil
}

// ConfigFromEnv builds the environment configuration
// (AGENTGATE_BACKUP_S3_*). It returns ErrNotConfigured when no
// endpoint is set; a set endpoint with missing fields is a
// validation error, not silence.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	endpoint := getenv("AGENTGATE_BACKUP_S3_ENDPOINT")
	if endpoint == "" {
		return Config{}, ErrNotConfigured
	}
	cfg := Config{
		Endpoint:  endpoint,
		Bucket:    getenv("AGENTGATE_BACKUP_S3_BUCKET"),
		Region:    getenv("AGENTGATE_BACKUP_S3_REGION"),
		AccessKey: getenv("AGENTGATE_BACKUP_S3_ACCESS_KEY"),
		SecretKey: getenv("AGENTGATE_BACKUP_S3_SECRET_KEY"),
		Source:    "env",
	}
	cfg = cfg.WithDefaults()
	if v := getenv("AGENTGATE_BACKUP_STATE_INTERVAL_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("AGENTGATE_BACKUP_STATE_INTERVAL_HOURS=%q invalid, want positive hours", v)
		}
		cfg.StateInterval = time.Duration(n) * time.Hour
	}
	if v := getenv("AGENTGATE_BACKUP_STATE_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("AGENTGATE_BACKUP_STATE_RETENTION_DAYS=%q invalid, want positive days", v)
		}
		cfg.StateRetention = time.Duration(n) * 24 * time.Hour
	}
	if v := getenv("AGENTGATE_BACKUP_AUDIT_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("AGENTGATE_BACKUP_AUDIT_RETENTION_DAYS=%q invalid, want positive days", v)
		}
		cfg.AuditRetention = time.Duration(n) * 24 * time.Hour
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// getenvOS is ConfigFromEnv over the process environment, for the
// CLI and the server bootstrap.
func getenvOS(key string) string { return os.Getenv(key) }

// EnvConfig is ConfigFromEnv over os.Getenv.
func EnvConfig() (Config, error) { return ConfigFromEnv(getenvOS) }
