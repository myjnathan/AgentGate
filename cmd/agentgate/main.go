// AgentGate server: a single Go binary serving the web console (/),
// the management API (/api), the OpenAI-compatible model gateway
// (/v1), the remote MCP server (/mcp) and — in demo mode only — a mock
// model upstream (/mock).
//
// Environment:
//
//	AGENTGATE_ADDR        listen address (default 127.0.0.1:8787)
//	AGENTGATE_DATA        data directory (default ./data)
//	AGENTGATE_MASTER_KEY  master secret for encrypting channel keys (required, >=16 chars)
//	AGENTGATE_DEMO        "1" seeds the PoC demo company on first run
//	AGENTGATE_AUDIT_RETENTION_DAYS
//	                     audit retention policy in days (default 180);
//	                     events older than now minus this many days are
//	                     eligible for archiving via POST /api/audit/archive
//	AGENTGATE_OIDC_ISSUER / AGENTGATE_OIDC_CLIENT_ID /
//	AGENTGATE_OIDC_CLIENT_SECRET
//	                     enable OpenID Connect login (backlog F9); all
//	                     three must be set. Optional: AGENTGATE_OIDC_REDIRECT_URL,
//	                     AGENTGATE_OIDC_SCOPES, AGENTGATE_OIDC_DEPARTMENT_CLAIM.
//	                     Local login always stays available as a fallback.
//	AGENTGATE_LDAP_URL / AGENTGATE_LDAP_BIND_DN /
//	AGENTGATE_LDAP_BIND_PASSWORD / AGENTGATE_LDAP_BASE_DN
//	                     LDAP directory configuration (backlog F9);
//	                     config only in this round, no LDAP client yet
//	                     (see docs/ldap.md).
//	AGENTGATE_CACHE_ENABLED / AGENTGATE_CACHE_TTL_SECONDS
//	                     exact-match response cache (backlog F11):
//	                     enabled by default; set ENABLED=0/false to
//	                     disable at boot, TTL_SECONDS sets the cache
//	                     lifetime in seconds (default 3600, minimum
//	                     60). Admins can toggle/clear it at runtime
//	                     from the console; the cache is in-memory and
//	                     resets on restart.
//	OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_SERVICE_NAME /
//	OTEL_EXPORTER_OTLP_PROTOCOL
//	                     OpenTelemetry export (backlog F16): set the
//	                     endpoint to an OTLP/HTTP collector (e.g.
//	                     http://localhost:4318) to export desensitized
//	                     traces and metrics; unset = tracing disabled.
//	                     Protocol must be http/json when set.
//	AGENTGATE_SEMANTIC_MODEL_URL / AGENTGATE_SEMANTIC_MODEL_TOKEN /
//	AGENTGATE_SEMANTIC_MODEL
//	                     semantic-entry model generation (backlog F19):
//	                     when set, POST /api/kb/folders/{id}/semantic
//	                     with {"model": true} asks this chat-completions
//	                     endpoint to write FRAS entries; unset = model
//	                     generation unavailable (extractive only).
//	AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS
//	                     exception approval flow (backlog F24): set
//	                     to "1" to let top-secret calls be excepted;
//	                     unset/anything else keeps them closed.
//	AGENTGATE_HA_MODE        high availability (backlog F27): "active"
//	                     (default) fails fast if another instance
//	                     already holds the data-directory lock;
//	                     "standby" waits for the lock and takes over
//	                     when the current holder dies. See
//	                     docs/ha-design.md.
//	AGENTGATE_FSYNC         durable writes (backlog F27 phase 2): set
//	                     to "1" to fsync every audit append and every
//	                     state save, narrowing the hard-power-loss
//	                     RPO to the last completed write. Default off:
//	                     fsync costs one disk sync per call, and a
//	                     plain process crash loses nothing either way.
//	AGENTGATE_BACKUP_S3_ENDPOINT / AGENTGATE_BACKUP_S3_BUCKET /
//	AGENTGATE_BACKUP_S3_REGION / AGENTGATE_BACKUP_S3_ACCESS_KEY /
//	AGENTGATE_BACKUP_S3_SECRET_KEY
//	                     object-storage backup (backlog F27 phase 3
//	                     B1): S3-compatible endpoint + bucket +
//	                     credentials for hourly state snapshots and
//	                     audit archive segments. The endpoint must be
//	                     self-hosted on the same machine or intranet
//	                     (Garage, rclone serve s3, or an existing
//	                     MinIO) — never a cloud bucket. Unset = no
//	                     periodic uploads; the console can store the
//	                     same config encrypted instead (POST
//	                     /api/backup/config). Optional:
//	                     AGENTGATE_BACKUP_STATE_INTERVAL_HOURS
//	                     (default 1),
//	                     AGENTGATE_BACKUP_STATE_RETENTION_DAYS
//	                     (default 7),
//	                     AGENTGATE_BACKUP_AUDIT_RETENTION_DAYS
//	                     (default 90). See docs/backup.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/backup"
	"github.com/myjnathan/agentgate/internal/distrib"
	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/gateway"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/ha"
	"github.com/myjnathan/agentgate/internal/mcp"
	"github.com/myjnathan/agentgate/internal/mockllm"
	"github.com/myjnathan/agentgate/internal/oidc"
	"github.com/myjnathan/agentgate/internal/onboard"
	"github.com/myjnathan/agentgate/internal/orgsync"
	"github.com/myjnathan/agentgate/internal/otel"
	"github.com/myjnathan/agentgate/internal/restadapter"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/semidx"
	"github.com/myjnathan/agentgate/internal/store"
	"github.com/myjnathan/agentgate/internal/terminal"
	"github.com/myjnathan/agentgate/internal/webui"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Backup CLI (backlog F27 phase 3 B1): `agentgate backup ...`
	// runs without a server and without the master key — restore
	// must work when the data directory is empty. See backup_cli.go.
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		os.Exit(runBackupCLI(os.Args[2:]))
	}
	addr := envOr("AGENTGATE_ADDR", "127.0.0.1:8787")
	dataDir := envOr("AGENTGATE_DATA", "./data")
	master := os.Getenv("AGENTGATE_MASTER_KEY")
	if len(master) < 16 {
		log.Fatal("AGENTGATE_MASTER_KEY must be set (>=16 chars)")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	// Single-writer lock on the data directory (backlog F27): two
	// processes must never write state.json / audit.jsonl at once, or
	// the audit chain forks. The lock is taken before any shared file
	// is opened; the kernel releases it when the holder dies, so a
	// standby node takes over by simply acquiring it.
	haMode, err := ha.ParseMode(os.Getenv("AGENTGATE_HA_MODE"))
	if err != nil {
		log.Fatal(err)
	}
	var haLock *ha.Lock
	var waited bool
	warmups := 0
	if haMode == ha.ModeStandby {
		// Standby warm-up (backlog F27 phase 2): while waiting for
		// the lock, re-read state.json read-only every 5 seconds, so
		// the snapshot is already parsed and fresh in the page cache
		// when takeover happens. The primary writes state.json by
		// atomic rename, so a warm-up read always sees a whole
		// snapshot, never a torn file; a stale or unreadable snapshot
		// only logs a line and never blocks takeover.
		haLock, waited, err = ha.AcquireStandby(dataDir, 5*time.Second, func() error {
			warmups++
			b, rerr := os.ReadFile(filepath.Join(dataDir, "state.json"))
			if rerr != nil {
				if !os.IsNotExist(rerr) {
					log.Printf("ha: standby warmup: state.json unreadable: %v", rerr)
				}
				return rerr
			}
			var st store.State
			if jerr := json.Unmarshal(b, &st); jerr != nil {
				log.Printf("ha: standby warmup: state.json unparsable: %v", jerr)
				return jerr
			}
			log.Printf("ha: standby warmup: state.json %d bytes, %d users, %d channels", len(b), len(st.Users), len(st.Channels))
			return nil
		})
	} else {
		haLock, waited, err = ha.Acquire(dataDir, haMode)
	}
	if err != nil {
		if errors.Is(err, ha.ErrLocked) {
			log.Fatalf("another AgentGate instance holds the data directory lock (%s); start this node with AGENTGATE_HA_MODE=standby to wait for takeover", dataDir)
		}
		log.Fatal(err)
	}
	defer haLock.Release()
	if waited {
		log.Printf("ha: took over as active (previous holder released the data-directory lock; %d warmup reads while standing by)", warmups)
	}
	st, err := store.New(dataDir)
	if err != nil {
		log.Fatal(err)
	}
	box, err := secretbox.New(master)
	if err != nil {
		log.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(dataDir, "audit.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	// Durable writes (backlog F27 phase 2): AGENTGATE_FSYNC=1 fsyncs
	// every audit append and every state save. Default off: one disk
	// sync per call, and a plain process crash loses nothing either
	// way — the option only narrows the hard-power-loss window.
	if fsync := os.Getenv("AGENTGATE_FSYNC"); fsync == "1" || strings.EqualFold(fsync, "true") {
		st.SetSync(true)
		al.SetSync(true)
		log.Printf("ha: durable writes enabled (AGENTGATE_FSYNC=1)")
	}
	// Verify the audit chain before serving: a standby that takes over
	// must continue the old chain, never extend a broken one. A broken
	// chain refuses takeover outright (fail-closed); a normal (active)
	// start only warns, so an existing single-node install is never
	// bricked by an upgrade, but /readyz reports it not-ready.
	chainOK := al.Verify()
	if !chainOK {
		log.Printf("WARNING: audit hash chain verification failed; standby takeover refuses a broken chain (see docs/ha-design.md for recovery)")
	}
	if haMode == ha.ModeStandby && !chainOK {
		log.Fatal("ha: standby takeover refused: audit hash chain verification failed; restore the audit log before starting a standby")
	}
	retentionDays := 180
	if v := os.Getenv("AGENTGATE_AUDIT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			retentionDays = n
		} else {
			log.Printf("AGENTGATE_AUDIT_RETENTION_DAYS=%q invalid, using default 180", v)
		}
	}
	al.SetRetentionDays(retentionDays)
	au := auth.New(st)

	selfBase := "http://" + addr
	demo := os.Getenv("AGENTGATE_DEMO") == "1"
	if demo {
		if err := seed.Demo(st, box, selfBase); err != nil {
			log.Printf("seed: %v (continuing)", err)
		} else {
			log.Printf("demo company seeded: 星河精密 (admin/agentgate-admin, employees/demo12345)")
		}
	}

	banned := splitEnv("AGENTGATE_BANNED_WORDS", seed.DemoBannedWords)
	custom := splitEnv("AGENTGATE_CUSTOM_WORDS", seed.DemoCustomWords)
	// Launch phase (backlog F34): persisted in state; a fresh
	// install (no users yet) starts in observe so day-one mistakes
	// cost nothing, an existing install keeps block (its previous
	// behaviour), and demo stays block (the PoC demonstrates
	// blocks). A template applied earlier re-applies its custom
	// words on top of the env lists, so a restart never loses them.
	var phase string
	var conservative bool
	st.Read(func(s *store.State) {
		if s.Onboarding != nil {
			phase = s.Onboarding.LaunchPhase
			conservative = s.Onboarding.ConservativeApplied
		}
		if phase == "" {
			if demo {
				phase = onboard.PhaseBlock
			} else if len(s.Users) == 0 {
				phase = onboard.PhaseObserve
			} else {
				phase = onboard.PhaseBlock
			}
		}
	})
	var appliedCustom []string
	st.Read(func(s *store.State) {
		if s.Onboarding != nil {
			appliedCustom = s.Onboarding.AppliedCustomWords
		}
	})
	custom = append(append([]string(nil), custom...), appliedCustom...)
	seen := map[string]bool{}
	deduped := custom[:0]
	for _, w := range custom {
		if !seen[w] {
			seen[w] = true
			deduped = append(deduped, w)
		}
	}
	custom = deduped
	// F35: console-managed DLP lists override the boot lists
	// (env + template words) — once a list is managed from the
	// DLP page, the page is its source of truth across restarts.
	var dlp *store.DLPWordLists
	st.Read(func(s *store.State) { dlp = s.DLP })
	banned, custom = dlp.Effective(banned, custom)
	st.WithLock(func(s *store.State) {
		if s.Onboarding == nil {
			s.Onboarding = &store.OnboardingState{}
		}
		if s.Onboarding.LaunchPhase == "" {
			s.Onboarding.LaunchPhase = phase
		}
	})
	_ = st.Save()
	redactForTier := map[store.Tier]bool{store.TierManaged: true}
	if conservative {
		// The conservative preset redacts both tiers (F34).
		redactForTier[store.TierBYOA] = true
	}
	det := guard.NewDetector(banned, custom)
	// Response cache (backlog F11): exact-match cache for clean
	// public/internal chat completions; Cloudflare's TTL floor is
	// 60 seconds, so anything smaller falls back to the default.
	cacheTTL := 3600 * time.Second
	if v := os.Getenv("AGENTGATE_CACHE_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 60 {
			cacheTTL = time.Duration(n) * time.Second
		} else {
			log.Printf("AGENTGATE_CACHE_TTL_SECONDS=%q invalid, using default 3600", v)
		}
	}
	// OpenTelemetry export (backlog F16): disabled unless an OTLP
	// endpoint is configured; a bad config only logs, never stops
	// the server — observability must not take down the gateway.
	otelCfg := otel.ConfigFromEnv(os.Getenv)
	otelCfg.ServiceVersion = version
	otelTracer := otel.New(otelCfg)
	if otelTracer.Enabled() {
		log.Printf("OpenTelemetry enabled: OTLP/HTTP endpoint %s service %s", otelCfg.Endpoint, otelCfg.ServiceNameOrDefault())
	} else if err := otelCfg.Validate(); err != nil {
		log.Printf("OpenTelemetry disabled: %v", err)
	}
	otelTracer.Run()
	defer otelTracer.Shutdown()

	gw := gateway.New(st, au, box, al, gateway.Policy{
		BannedWords:   banned,
		CustomWords:   custom,
		RedactForTier: redactForTier,
		LaunchPhase:   phase,
		CacheTTL:      cacheTTL,
		Tracer:        otelTracer,
	})
	if v := os.Getenv("AGENTGATE_CACHE_ENABLED"); v == "0" || strings.EqualFold(v, "false") {
		gw.Cache().SetEnabled(false)
	}
	// Webhook alerts (backlog F5): every audit event flows through the
	// notify hook; only alertable kinds (blocks, cooldowns, lockouts)
	// are delivered. Delivery reports are Kind "alert" and therefore
	// never re-trigger delivery.
	dispatcher := alert.New(st, box, alert.WithOnResult(func(r alert.Result) {
		action := "alert_delivered"
		detail := map[string]string{"webhook_id": r.Webhook.ID, "webhook_name": r.Webhook.Name,
			"attempts": strconv.Itoa(r.Attempts), "alert": r.Alert.Action}
		if r.Err != nil {
			action = "alert_delivery_failed"
			detail["error"] = alert.ErrClass(r.Err, r.Status)
		}
		al.Append(audit.Event{Actor: "system", Kind: "alert", Action: action, Detail: detail})
	}))
	dispatcher.Start()
	defer dispatcher.Stop()
	al.SetNotifyHook(dispatcher.Notify)

	fed := federation.New(st, box)
	restAd := restadapter.New(st, box)
	// OIDC login (backlog F9): enabled only when issuer + client ID +
	// client secret are all set; a discovery failure disables OIDC
	// with a log line instead of refusing to start (local login is
	// the fallback).
	var oidcProvider *oidc.Provider
	if oidcCfg, enabled := oidc.ConfigFromEnv(os.Getenv); enabled {
		p, err := oidc.NewProvider(oidcCfg)
		if err != nil {
			log.Printf("OIDC disabled: %v", err)
		} else {
			oidcProvider = p
			log.Printf("OIDC enabled: issuer %s (local login still available)", oidcCfg.Issuer)
			// F26 role mapping: the admin-configured group -> role
			// map from AGENTGATE_OIDC_ROLE_MAP. Groups are checked
			// in sorted order so a user in several mapped groups
			// always gets the same role; unmapped groups yield no
			// role (the auth layer falls back to employee).
			if len(oidcCfg.RoleMap) > 0 {
				roleMap := oidcCfg.RoleMap
				au.RoleFromGroups = func(groups []string) (string, bool) {
					sorted := append([]string(nil), groups...)
					sort.Strings(sorted)
					for _, g := range sorted {
						if r, ok := roleMap[g]; ok {
							return r, true
						}
					}
					return "", false
				}
				log.Printf("OIDC role mapping enabled: %d group(s)", len(roleMap))
			}
		}
	}
	// Folder knowledge base (backlog F18): the in-memory index over
	// registered shared folders. Indexed at startup (after the demo
	// seed above, so demo folders are included) and on admin rescan.
	fk := folderkb.New(st)
	// Semantic index (backlog F19): FRAS entries over the folder
	// index, regenerated extractively after every rescan.
	semix := semidx.New()
	mcpSrv := mcp.New(st, au, al, det, fed, restAd)
	mcpSrv.SetFolderKB(fk)
	mcpSrv.SetSemanticIndex(semix)
	mcpSrv.SetTracer(otelTracer)
	// F39 slice 2: the session-suspicion tracker is shared by the
	// model gateway (which marks) and the MCP gateway (which
	// enforces the action fallback on high-risk tool calls).
	mcpSrv.SetInjectionSuspicion(gw.InjectionSuspicion())
	rest := api.New(st, au, box, al)
	rest.SetTracer(otelTracer)
	// First-run onboarding (backlog F34): the gateway's runtime
	// setters, so the console can switch phases, apply templates
	// and add template words without a restart. In demo mode the
	// seed already created users, so no setup code is issued.
	rest.SetLaunchPhaseApplier(gw.SetLaunchPhase)
	rest.SetRedactForTierApplier(gw.SetRedactForTier)
	rest.SetWordsApplier(gw.SetWords)
	rest.SetWordsReader(gw.Words)
	if !demo {
		var noUsers bool
		st.Read(func(s *store.State) { noUsers = len(s.Users) == 0 })
		if noUsers {
			code := onboard.NewSetupCode()
			if code != "" {
				rest.SetSetupCode(code)
				log.Printf("first-run setup code: %s — open the web console and enter this code to create the first administrator (single-use; five failed attempts void it, restart to issue a new one)", code)
			}
		}
	}
	rest.SetOIDC(oidcProvider)
	rest.SetFederation(fed)
	rest.SetRESTAdapter(restAd)
	rest.SetFolderKB(fk)
	rest.SetSemanticIndex(semix)
	if semURL := os.Getenv("AGENTGATE_SEMANTIC_MODEL_URL"); semURL != "" {
		rest.SetSemanticModel(semidx.NewModelGenerator(semidx.ModelConfig{
			Endpoint: semURL, Token: os.Getenv("AGENTGATE_SEMANTIC_MODEL_TOKEN"),
			Model: os.Getenv("AGENTGATE_SEMANTIC_MODEL")}, nil))
	}
	rest.SetCache(gw.Cache())
	rest.SetSemReviewStats(gw.SemReviewStats)
	rest.SetInjectionStats(gw.InjectionStats)
	rest.SetAutoRouteStats(gw.AutoRouteStats)
	rest.SetAlertDispatcher(dispatcher)
	// Terminal inventory (backlog F23): the sidecar policy snapshot
	// is the gateway's own word lists, so a sidecar that refreshes
	// from it enforces exactly what the server enforces. The
	// offline ceiling defaults to internal (fail-closed above it)
	// and can be raised via AGENTGATE_OFFLINE_MAX_LEVEL.
	offlineMax := terminal.DefaultOfflineMaxLevel
	if v := os.Getenv("AGENTGATE_OFFLINE_MAX_LEVEL"); v != "" {
		if terminal.ParseLevel(v) {
			offlineMax = v
		} else {
			log.Printf("AGENTGATE_OFFLINE_MAX_LEVEL=%q invalid, using default internal", v)
		}
	}
	// The snapshot reads the gateway's live words and phase (F34):
	// a template applied at runtime reaches sidecars on their next
	// refresh, and in observe phase the sidecar observes too
	// instead of blocking on the endpoint.
	rest.SetSidecarPolicy(func() terminal.PolicySnapshot {
		banned, custom := gw.Words()
		return terminal.NewPolicySnapshot(banned, custom, gw.LaunchPhase() == onboard.PhaseObserve, offlineMax)
	})
	// Exception approval flow (backlog F24): top-secret exceptions
	// stay closed unless the operator explicitly opts in — the
	// default posture is that secret data can never be excepted.
	if os.Getenv("AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS") == "1" {
		rest.SetAllowTopSecretExceptions(true)
		log.Printf("top-secret exceptions enabled (AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS=1)")
	}
	// Package distribution (backlog F20): install packages are
	// hosted under <data>/packages, employees download them, and
	// their hashes feed the F14 attestation baseline.
	rest.SetPackageRegistry(distrib.NewRegistry(filepath.Join(dataDir, "packages")))
	// Organisation sync (backlog F21): third-party directories
	// (DingTalk / Feishu / WeCom) sync into platform accounts; a
	// departed employee's sessions are revoked when the sync
	// disables the account.
	rest.SetOrgSync(orgsync.New(st, au.RevokeUserSessions))
	for _, src := range fk.Sources() {
		if !src.Enabled {
			continue
		}
		stats, err := fk.Rescan(src)
		if err != nil {
			log.Printf("folderkb: %s (%s): %v", src.Name, src.Path, err)
			continue
		}
		log.Printf("folderkb: %s: %d docs (%d added, %d updated, %d unchanged, %d errors)",
			src.Name, fk.DocCount(src.ID), stats.Added, stats.Updated, stats.Unchanged, stats.Errors)
	}
	if reg, _ := semix.RegenerateChanged(context.Background(), fk.Snapshots()); reg > 0 {
		log.Printf("semidx: generated semantic entries for %d documents", reg)
	}
	// Object-storage backup (backlog F27 phase 3 B1): state snapshots
	// hourly + audit archive segments to a self-hosted S3-compatible
	// bucket (same machine or intranet — never a cloud bucket). A
	// stored console config wins over environment variables; the
	// secret key is decrypted only in memory, never logged. The
	// periodic runner re-reads the config every cycle, so a console
	// change takes effect without a restart.
	backupSvc := backup.NewService(dataDir, func() (backup.Config, error) {
		var stored *store.BackupConfig
		st.Read(func(s *store.State) { stored = s.Backup })
		if stored != nil && stored.Endpoint != "" {
			secret, err := box.Decrypt(stored.SecretEnc)
			if err != nil {
				return backup.Config{}, fmt.Errorf("decrypt backup secret: %w", err)
			}
			cfg := backup.Config{Endpoint: stored.Endpoint, Bucket: stored.Bucket,
				Region: stored.Region, AccessKey: stored.AccessKey,
				SecretKey: secret, Source: "stored"}.WithDefaults()
			if stored.StateIntervalHours > 0 {
				cfg.StateInterval = time.Duration(stored.StateIntervalHours) * time.Hour
			}
			if stored.StateRetentionDays > 0 {
				cfg.StateRetention = time.Duration(stored.StateRetentionDays) * 24 * time.Hour
			}
			if stored.AuditRetentionDays > 0 {
				cfg.AuditRetention = time.Duration(stored.AuditRetentionDays) * 24 * time.Hour
			}
			if err := cfg.Validate(); err != nil {
				return backup.Config{}, err
			}
			return cfg, nil
		}
		return backup.ConfigFromEnv(os.Getenv)
	}, st.SnapshotBytes, func(ev audit.Event) { al.Append(ev) }, func(rep backup.Report) {
		st.WithLock(func(s *store.State) {
			if s.Backup == nil {
				s.Backup = &store.BackupConfig{}
			}
			s.Backup.LastRun = &store.BackupRunSummary{
				StartedAt: rep.StartedAt, FinishedAt: rep.FinishedAt,
				StateUploaded: rep.StateUploaded, StateKey: rep.StateKey,
				AuditUploaded: len(rep.AuditUploaded), AuditSkipped: rep.AuditSkipped,
				Pruned: len(rep.Pruned), Error: rep.Error,
			}
		})
		_ = st.Save()
	}, log.Printf)
	rest.SetBackupService(backupSvc)
	if _, err := backupSvc.EffectiveConfig(); err == nil {
		log.Printf("backup: object-storage backup enabled (state snapshots + audit segments; see docs/backup.md)")
	} else if errors.Is(err, backup.ErrNotConfigured) {
		log.Printf("backup: object storage not configured (set AGENTGATE_BACKUP_S3_* or POST /api/backup/config); periodic uploads disabled")
	} else {
		log.Printf("backup: config error: %v (periodic uploads disabled)", err)
	}
	go backupSvc.RunPeriodic(context.Background())

	mux := http.NewServeMux()
	mux.Handle("/", webui.Handler())
	rest.Register(mux)
	mux.HandleFunc("GET /v1/models", gw.Models)
	mux.HandleFunc("POST /v1/chat/completions", gw.ChatCompletions)
	mux.Handle("/mcp", mcpSrv)
	if demo {
		mux.Handle("/mock/", mockllm.Handler())
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Readiness probe (backlog F27): liveness (/healthz) only says the
	// process is alive; /readyz says this node can take traffic — its
	// state saves and audit appends are landing, and the audit chain
	// verified at startup. Load balancers drain a not-ready node
	// instead of restarting it; see docs/ha-design.md.
	mux.HandleFunc("GET /readyz", ha.ReadyHandler(st.LastSaveError, al.LastError, chainOK))

	log.Printf("AgentGate %s listening on %s (data: %s, demo: %v, audit retention: %d days)", version, addr, dataDir, demo, retentionDays)
	srv := &http.Server{Addr: addr, Handler: securityHeaders(mux)}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func splitEnv(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
