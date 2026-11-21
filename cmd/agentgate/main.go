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
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/gateway"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/mcp"
	"github.com/myjnathan/agentgate/internal/mockllm"
	"github.com/myjnathan/agentgate/internal/oidc"
	"github.com/myjnathan/agentgate/internal/restadapter"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
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
	gw := gateway.New(st, au, box, al, gateway.Policy{
		BannedWords:   banned,
		CustomWords:   custom,
		RedactForTier: map[store.Tier]bool{store.TierManaged: true},
	})
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
	mcpSrv := mcp.New(st, au, al, det, fed, restAd)
	mcpSrv.SetFolderKB(fk)
	rest := api.New(st, au, box, al)
	rest.SetOIDC(oidcProvider)
	rest.SetFederation(fed)
	rest.SetRESTAdapter(restAd)
	rest.SetFolderKB(fk)
	rest.SetAlertDispatcher(dispatcher)
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
