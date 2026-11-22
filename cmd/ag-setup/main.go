// ag-setup is the employee-side onboarding tool (thin adapter layer).
//
// It runs the device-code login against an AgentGate server, then
// detects locally installed desktop agents and writes gateway
// configuration for each one it knows how to configure:
//
//   - DSH          -> ~/.agentgate/agents/dsh.json + prints dsh plugin config
//   - Claude Code  -> ~/.claude/agentgate-mcp.json (MCP server entry)
//   - Codex        -> ~/.agentgate/agents/codex.json
//
// The platform contract never depends on these adapters; they only
// write configuration. Any agent can also be configured manually:
// model base URL = <server>/v1, MCP URL = <server>/mcp, token = ag_at_…
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	server := flag.String("server", "http://127.0.0.1:8787", "AgentGate server base URL")
	agent := flag.String("agent", "dsh", "agent type: dsh | claude-code | codex | workbuddy | other")
	tier := flag.String("tier", "byoa", "requested tier: byoa | managed")
	// Attestation (backlog F14): the evidence this agent reports with
	// its login. The config hash covers the gateway configuration
	// below (never the token); the binary hash covers this executable.
	version := flag.String("version", "0.1.0", "agent version reported for attestation")
	plugins := flag.String("plugins", "", "installed plugins, comma-separated")
	guard := flag.Bool("guard", false, "report that egress guard rules are applied")
	flag.Parse()

	// 1. Start device code.
	var start struct {
		Code   string `json:"code"`
		Secret string `json:"secret"`
	}
	if err := postJSON(*server+"/api/setup/device/start", map[string]any{"agent_type": *agent, "tier": *tier}, &start); err != nil {
		fatal("start device login: %v", err)
	}
	fmt.Printf("请打开 AgentGate 网页并登录，在「批准设备码」处输入: %s\n", start.Code)

	// 1b. Submit attestation, bound to this device code. A failed
	// submission does not stop the login: the platform decides at
	// approval time (a managed request without a passing report is
	// downgraded to BYOA, not refused).
	submitAttestation(*server, start.Code, start.Secret, *agent, *tier, *version, *plugins, *guard)
	fmt.Println("等待批准中…")

	// 2. Poll until approved.
	var token string
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		var poll struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}
		if err := postJSON(*server+"/api/setup/device/poll", map[string]any{"code": start.Code, "secret": start.Secret}, &poll); err != nil {
			fatal("poll: %v", err)
		}
		switch poll.Status {
		case "approved":
			token = poll.Token
			goto approved
		case "denied", "expired":
			fatal("login %s", poll.Status)
		}
		time.Sleep(2 * time.Second)
	}
	fatal("timed out waiting for approval")
approved:
	fmt.Println("批准成功，正在写入 Agent 配置…")

	// 3. Write config for detected agents.
	home, _ := os.UserHomeDir()
	cfg := map[string]any{
		"server":        *server,
		"model_base":    *server + "/v1",
		"mcp_url":       *server + "/mcp",
		"token":         token,
		"agent_type":    *agent,
		"tier":          *tier,
		"configured_at": time.Now().Format(time.RFC3339),
	}
	wrote := []string{}
	agDir := filepath.Join(home, ".agentgate", "agents")
	_ = os.MkdirAll(agDir, 0o700)
	// Generic record (used by all adapters and for support).
	if writeJSONFile(filepath.Join(agDir, *agent+".json"), cfg) == nil {
		wrote = append(wrote, filepath.Join(agDir, *agent+".json"))
	}
	// Claude Code style MCP config snippet.
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		mcpCfg := map[string]any{"mcpServers": map[string]any{
			"agentgate": map[string]any{"type": "http", "url": *server + "/mcp",
				"headers": map[string]string{"Authorization": "Bearer " + token}},
		}}
		p := filepath.Join(home, ".claude", "agentgate-mcp.json")
		if writeJSONFile(p, mcpCfg) == nil {
			wrote = append(wrote, p+" (merge into .claude.json mcpServers)")
		}
	}
	for _, p := range wrote {
		fmt.Println("  wrote:", p)
	}
	fmt.Println("完成。今后在你的 Agent 中：模型地址指向 " + *server + "/v1，MCP 指向 " + *server + "/mcp。")
}

// submitAttestation reports this agent's evidence for its device-code
// login (backlog F14). The config hash is taken over the gateway
// configuration ag-setup is about to write, minus the token (which
// does not exist yet) — it proves the agent was configured from the
// enterprise template, and it never contains a secret.
func submitAttestation(server, code, secret, agent, tier, version, plugins string, guard bool) {
	cfgHash := configHash(server, agent, tier)
	var pluginList []string
	for _, p := range strings.Split(plugins, ",") {
		if p = strings.TrimSpace(p); p != "" {
			pluginList = append(pluginList, p)
		}
	}
	binHash := ""
	if exe, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(exe); err == nil {
			sum := sha256.Sum256(b)
			binHash = hex.EncodeToString(sum[:])
		}
	}
	report := map[string]any{
		"version": version, "config_hash": cfgHash, "binary_hash": binHash,
		"plugins": pluginList, "guard_applied": guard, "os": runtime.GOOS,
	}
	var out struct {
		Verdict struct {
			Status  string   `json:"status"`
			Reasons []string `json:"reasons,omitempty"`
		} `json:"verdict"`
	}
	if err := postJSON(server+"/api/setup/device/attest",
		map[string]any{"code": code, "secret": secret, "report": report}, &out); err != nil {
		fmt.Printf("证明提交失败（登录继续，批准时可能被降级为自带级）: %v\n", err)
		return
	}
	fmt.Printf("Agent 证明已提交，裁决: %s", out.Verdict.Status)
	if len(out.Verdict.Reasons) > 0 {
		fmt.Printf("（%s）", strings.Join(out.Verdict.Reasons, ","))
	}
	fmt.Println()
}

// configHash is the SHA-256 of the canonical gateway configuration
// (server / model_base / mcp_url / agent_type / tier), the same
// values ag-setup writes into the agent config file, minus the
// token. json.Marshal sorts map keys, so the hash is deterministic.
func configHash(server, agent, tier string) string {
	b, _ := json.Marshal(map[string]any{
		"server": server, "model_base": server + "/v1", "mcp_url": server + "/mcp",
		"agent_type": agent, "tier": tier,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func postJSON(url string, in, out any) error {
	b, _ := json.Marshal(in)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func writeJSONFile(path string, v any) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	return os.WriteFile(path, b, 0o600)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ag-setup: "+format+"\n", args...)
	os.Exit(1)
}
