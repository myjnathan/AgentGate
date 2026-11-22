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
	if os.Getenv("AGENTGATE_GUARD_DRY_RUN") == "1" || strings.EqualFold(os.Getenv("AGENTGATE_GUARD_DRY_RUN"), "true") {
		cfg.DryRun = true
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

	srv, err := sidecar.New(cfg)
	if err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
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
