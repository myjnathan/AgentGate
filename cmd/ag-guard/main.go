// Command ag-guard is the local privacy gateway daemon (backlog
// F15): the endpoint-side half of AgentGate's two-layer
// desensitization. It listens on 127.0.0.1 and proxies a desktop
// agent's model traffic to the enterprise gateway, redacting
// PII/credentials before anything leaves the machine and restoring
// placeholders in responses. The mapping never leaves this process.
//
// Run it, then point the agent's model base URL at it (or run
// `ag-setup --sidecar`, which writes that configuration):
//
//	ag-guard -gateway http://127.0.0.1:8787
//
// Configuration (in increasing precedence): ~/.agentgate/guard.json,
// environment variables, command-line flags.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/myjnathan/agentgate/internal/sidecar"
)

func main() {
	log.SetPrefix("ag-guard: ")
	gateway := flag.String("gateway", "", "enterprise gateway base URL (env AGENTGATE_GATEWAY)")
	listen := flag.String("listen", "", "loopback listen address (env AGENTGATE_GUARD_LISTEN)")
	configPath := flag.String("config", "", "JSON config file (default ~/.agentgate/guard.json when it exists)")
	dryRun := flag.Bool("dry-run", false, "record findings but never block or redact (env AGENTGATE_GUARD_DRY_RUN=1)")
	banned := flag.String("banned-words", "", "comma-separated banned words (env AGENTGATE_GUARD_BANNED_WORDS)")
	custom := flag.String("custom-words", "", "comma-separated custom sensitive words (env AGENTGATE_GUARD_CUSTOM_WORDS)")
	agentType := flag.String("agent-type", "", "agent type reported in heartbeats (env AGENTGATE_GUARD_AGENT_TYPE)")
	plugins := flag.String("plugins", "", "comma-separated installed plugins reported in heartbeats (env AGENTGATE_GUARD_PLUGINS)")
	heartbeatSecs := flag.Int("heartbeat-seconds", 0, "terminal heartbeat interval in seconds, 0 = default 60, negative disables (env AGENTGATE_GUARD_HEARTBEAT_SECONDS)")
	policyCache := flag.String("policy-cache", "", "path of the cached server policy file (env AGENTGATE_GUARD_POLICY_CACHE)")
	flag.Parse()

	cfg := sidecar.Config{GatewayURL: "http://127.0.0.1:8787", Listen: sidecar.DefaultListen}
	// 1. Config file (default path when present, or the given path).
	cfgFile := *configPath
	if cfgFile == "" {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, ".agentgate", "guard.json")
			if _, err := os.Stat(p); err == nil {
				cfgFile = p
			}
		}
	}
	if cfgFile != "" {
		fc, err := sidecar.LoadConfigFile(cfgFile)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		cfg = fc
		if cfg.Listen == "" {
			cfg.Listen = sidecar.DefaultListen
		}
	}
	// 2. Environment overrides.
	if v := os.Getenv("AGENTGATE_GATEWAY"); v != "" {
		cfg.GatewayURL = v
	}
	if v := os.Getenv("AGENTGATE_GUARD_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("AGENTGATE_GUARD_BANNED_WORDS"); v != "" {
		cfg.BannedWords = splitList(v)
	}
	if v := os.Getenv("AGENTGATE_GUARD_CUSTOM_WORDS"); v != "" {
		cfg.CustomWords = splitList(v)
	}
	if v := os.Getenv("AGENTGATE_GUARD_DRY_RUN"); v != "" {
		cfg.DryRun = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("AGENTGATE_GUARD_AGENT_TYPE"); v != "" {
		cfg.AgentType = v
	}
	if v := os.Getenv("AGENTGATE_GUARD_PLUGINS"); v != "" {
		cfg.Plugins = splitList(v)
	}
	if v := os.Getenv("AGENTGATE_GUARD_HEARTBEAT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.HeartbeatIntervalSecs = n
		}
	}
	// The policy cache (backlog F23) defaults to next to the guard
	// config when one was loaded, else ~/.agentgate/policy-cache.json.
	if cfg.PolicyCachePath == "" {
		if cfgFile != "" {
			cfg.PolicyCachePath = filepath.Join(filepath.Dir(cfgFile), "policy-cache.json")
		} else if home, err := os.UserHomeDir(); err == nil {
			cfg.PolicyCachePath = filepath.Join(home, ".agentgate", "policy-cache.json")
		}
	}
	if v := os.Getenv("AGENTGATE_GUARD_POLICY_CACHE"); v != "" {
		cfg.PolicyCachePath = v
	}
	// 3. Flag overrides.
	if *gateway != "" {
		cfg.GatewayURL = *gateway
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *banned != "" {
		cfg.BannedWords = splitList(*banned)
	}
	if *custom != "" {
		cfg.CustomWords = splitList(*custom)
	}
	if *dryRun {
		cfg.DryRun = true
	}
	if *agentType != "" {
		cfg.AgentType = *agentType
	}
	if *plugins != "" {
		cfg.Plugins = splitList(*plugins)
	}
	if *heartbeatSecs != 0 {
		cfg.HeartbeatIntervalSecs = *heartbeatSecs
	}
	if *policyCache != "" {
		cfg.PolicyCachePath = *policyCache
	}

	srv, err := sidecar.New(cfg)
	if err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	// Heartbeats + policy cache refresh (backlog F23).
	srv.StartBackground()
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		srv.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()
	fmt.Printf("ag-guard %s listening on %s, forwarding to %s (dry-run=%v)\n",
		sidecar.SidecarHeader, cfg.Listen, cfg.GatewayURL, cfg.DryRun)
	fmt.Printf("Point your agent's model base URL at http://%s/v1\n", cfg.Listen)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
