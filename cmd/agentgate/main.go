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
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/distrib"
	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/gateway"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/mcp"
	"github.com/myjnathan/agentgate/internal/mockllm"
	"github.com/myjnathan/agentgate/internal/oidc"
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
	addr := envOr("AGENTGATE_ADDR", "127.0.0.1:8787")
	dataDir := envOr("AGENTGATE_DATA", "./data")
	master := os.Getenv("AGENTGATE_MASTER_KEY")
	if len(master) < 16 {
		log.Fatal("AGENTGATE_MASTER_KEY must be set (>=16 chars)")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatal(err)
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
		RedactForTier: map[store.Tier]bool{store.TierManaged: true},
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
	rest := api.New(st, au, box, al)
	rest.SetTracer(otelTracer)
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
	rest.SetSidecarPolicy(func() terminal.PolicySnapshot {
		return terminal.NewPolicySnapshot(banned, custom, false, offlineMax)
	})
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
