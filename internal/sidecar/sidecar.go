// Package sidecar is the local privacy gateway (backlog F15): the
// endpoint-side half of AgentGate's two-layer desensitization.
//
// It runs on the employee machine as the `ag-guard` daemon, listening
// on 127.0.0.1, and sits between any desktop agent (anything that can
// point its model base URL at it) and the enterprise gateway:
//
//	desktop agent -> 127.0.0.1:18765 (ag-guard) -> enterprise gateway -> model
//
// Before a request leaves the machine it is scanned with the same
// detector the server uses (internal/guard): banned words block the
// request locally, PII/credential/custom findings are replaced with
// reversible placeholders. The placeholder -> original mapping lives
// only in memory for that one request and is never written to disk,
// never sent to the gateway, and never logged. The model response is
// restored on the way back, so the agent keeps working with real
// values while the upstream model only ever saw placeholders.
//
// Egress guard linkage: the sidecar dials exactly one origin — the
// configured enterprise gateway. It cannot forward anywhere else, so
// an agent pointed at the sidecar cannot reach a public model API
// directly; combined with the OS firewall templates in
// endpoint/egress (which restrict the agent process to the gateway
// and 127.0.0.1), this is the "可预防" half of the managed tier.
//
// Borrowed method (no code copied): Calcium-Ion/AstrLink's local
// privacy gateway — on-device detection, warn/block/redact policies,
// placeholder mapping kept on-device, response restoration, and a
// dry-run mode that only records findings. AstrLink's privacy engine
// is a Go core plus a Rust ONNX worker; detection here reuses this
// repository's own internal/guard instead, and the proxy, mapping
// lifecycle and streaming restore are implemented here in plain Go
// (standard library only).
//
// F23 adds the continuous-inventory half: the sidecar reports a
// periodic heartbeat (version / config hash / binary hash / plugin
// list) so the platform keeps a live terminal record instead of a
// login-time snapshot, and it caches the server policy (banned /
// custom words, dry-run, offline max level) on disk. While the
// gateway is unreachable it keeps enforcing the cached policy and
// fails closed above the offline max level (IP-guard's offline
// policy cache is the model); decisions made offline are buffered
// and backfilled through the next heartbeat.
//
// This round is rule-based detection only (blueprint: "sidecar 先只
// 做规则检测"); local-model detection is a later round.
package sidecar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/store"
	"github.com/myjnathan/agentgate/internal/terminal"
)

// Version is reported in the X-AgentGate-Sidecar header and /status.
const Version = "0.1.0"

// SidecarHeader is the header value the sidecar sends upstream so
// the gateway can record in audit that a call was endpoint-processed.
const SidecarHeader = "ag-guard/" + Version

// DefaultListen is the loopback address the daemon binds to.
const DefaultListen = "127.0.0.1:18765"

// Config configures one sidecar daemon.
type Config struct {
	// GatewayURL is the enterprise gateway base URL, the only origin
	// this daemon ever dials (egress guard by construction).
	GatewayURL string `json:"gateway_url"`
	// Listen is the loopback listen address, host:port.
	Listen string `json:"listen,omitempty"`
	// BannedWords block the request locally (never leaves the machine).
	BannedWords []string `json:"banned_words,omitempty"`
	// CustomWords are enterprise-sensitive words treated as confidential.
	CustomWords []string `json:"custom_words,omitempty"`
	// DryRun records findings but never blocks or redacts (rollout
	// mode: measure false positives before enforcing).
	DryRun bool `json:"dry_run,omitempty"`
	// AgentType identifies this endpoint's agent in heartbeats
	// ("dsh", "claude-code", ...). The server overwrites it from
	// the bearer token, so it is only a label when no token has
	// been seen yet; empty means "other".
	AgentType string `json:"agent_type,omitempty"`
	// Plugins are the agent's installed plugins, reported in
	// heartbeats so drift can judge them against the F14 policy.
	Plugins []string `json:"plugins,omitempty"`
	// HeartbeatIntervalSecs is the terminal heartbeat period
	// (backlog F23). 0 = default 60s; negative disables heartbeats.
	HeartbeatIntervalSecs int `json:"heartbeat_interval_seconds,omitempty"`
	// PolicyCachePath is where the cached server policy snapshot
	// lives (backlog F23). Empty disables the disk cache: the
	// sidecar then enforces only its startup word lists until a
	// refresh succeeds. The file holds word lists and a dry-run
	// flag — no secret — and is written 0600.
	PolicyCachePath string `json:"policy_cache_path,omitempty"`
}

// Validate checks the config. The gateway URL must be an http(s)
// URL without embedded credentials, and must not point back at the
// sidecar's own listen address (a forwarding loop).
func (c Config) Validate() error {
	if c.GatewayURL == "" {
		return fmt.Errorf("gateway_url is required")
	}
	u, err := url.Parse(c.GatewayURL)
	if err != nil {
		return fmt.Errorf("invalid gateway_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("gateway_url must be http(s)")
	}
	if u.User != nil {
		return fmt.Errorf("gateway_url must not embed credentials")
	}
	if u.Host == "" {
		return fmt.Errorf("gateway_url must have a host")
	}
	listen := c.Listen
	if listen == "" {
		listen = DefaultListen
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	// Loop check: gateway origin == sidecar listen address.
	gwHost := strings.ToLower(u.Host)
	if gwHost == strings.ToLower(listen) {
		return fmt.Errorf("gateway_url points back at the sidecar itself (%s)", listen)
	}
	return nil
}

// LoadConfigFile reads a JSON config file. A missing file is not an
// error: the caller falls back to flags/environment defaults.
func LoadConfigFile(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("invalid sidecar config %s: %w", path, err)
	}
	return c, nil
}

// Decision is one local policy decision. It carries categories and
// kinds only — never the matched text.
type Decision struct {
	Time       time.Time `json:"time"`
	Decision   string    `json:"decision"` // allow | redact | block | dry_run | offline_blocked | offline_unreachable
	Categories []string  `json:"categories,omitempty"`
	Model      string    `json:"model,omitempty"`
	DataLevel  string    `json:"data_level,omitempty"`
}

// Counters are the sidecar's local audit: counts only, no content.
type Counters struct {
	Requests         int64            `json:"requests"`
	Forwarded        int64            `json:"forwarded"`
	BlockedLocal     int64            `json:"blocked_local"`
	RedactedRequests int64            `json:"redacted_requests"`
	RestoredValues   int64            `json:"restored_values"`
	FindingsByKind   map[string]int64 `json:"findings_by_category"`
	// OfflineBlocked / OfflineUnreachable count decisions made while
	// the gateway was unreachable (backlog F23): fail-closed denials
	// vs retryable outages. Both are backfilled via heartbeat.
	OfflineBlocked     int64 `json:"offline_blocked"`
	OfflineUnreachable int64 `json:"offline_unreachable"`
}

// Status is the GET /status response.
type Status struct {
	Version    string     `json:"version"`
	GatewayURL string     `json:"gateway_url"`
	Listen     string     `json:"listen"`
	DryRun     bool       `json:"dry_run"`
	UptimeSecs int64      `json:"uptime_seconds"`
	Counters   Counters   `json:"counters"`
	Recent     []Decision `json:"recent_decisions,omitempty"`
	// PolicyVersion / PolicySource describe the cached server policy
	// (backlog F23): "local" = the startup word lists, "gateway" =
	// refreshed from the server, "cached" = loaded from disk.
	PolicyVersion    string     `json:"policy_version"`
	PolicySource     string     `json:"policy_source"`
	PolicyRefreshed  *time.Time `json:"policy_refreshed_at,omitempty"`
	HeartbeatEnabled bool       `json:"heartbeat_enabled"`
	LastHeartbeat    *time.Time `json:"last_heartbeat_at,omitempty"`
	// OfflineBuffered is the number of outage decisions waiting to
	// be backfilled through the next heartbeat.
	OfflineBuffered int `json:"offline_buffered"`
}

const maxRecentDecisions = 50

// offlineEntry is one decision made while the gateway was
// unreachable, buffered for the next heartbeat's backfill (backlog
// F23). Categories only — never matched text or request content.
// The buffer is memory-only: a sidecar restart during an outage
// loses the un-backfilled entries (documented in
// docs/terminal-inventory.md), but the policy cache on disk survives.
type offlineEntry struct {
	Time       time.Time
	Decision   string // offline_blocked | offline_unreachable
	Categories []string
	Level      string
}

// Server is the sidecar daemon: a loopback proxy with local
// detection, redaction and restore.
type Server struct {
	cfg      Config
	det      *guard.Detector
	detMu    sync.RWMutex // guards det across policy refreshes
	client   *http.Client
	shortCli *http.Client // 10s timeout for policy fetch + heartbeat
	mu       sync.Mutex
	started  time.Time
	counters Counters
	recent   []Decision

	// policyMu guards the cached server policy and the heartbeat
	// state (F23). lastToken is the most recent bearer token seen
	// on a proxied request — memory only, never written to disk —
	// and is what heartbeats and policy refreshes authenticate with.
	policyMu        sync.Mutex
	policy          terminal.PolicySnapshot
	policySource    string // local | gateway | cached
	policyRefreshed time.Time
	lastToken       string
	lastHeartbeat   time.Time
	offline         []offlineEntry
	binaryHash      string
	heartbeatEvery  time.Duration // <=0 disables heartbeats
	done            chan struct{}
	stopOnce        sync.Once
}

// policyRefreshInterval is how often a proxied request may trigger a
// background policy refresh (at most once per interval).
const policyRefreshInterval = 5 * time.Minute

// New builds a Server from a validated config.
func New(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	interval := 60 * time.Second
	if cfg.HeartbeatIntervalSecs < 0 {
		interval = 0
	} else if cfg.HeartbeatIntervalSecs > 0 {
		interval = time.Duration(cfg.HeartbeatIntervalSecs) * time.Second
	}
	s := &Server{
		cfg:            cfg,
		det:            guard.NewDetector(cfg.BannedWords, cfg.CustomWords),
		client:         &http.Client{Timeout: 10 * time.Minute},
		shortCli:       &http.Client{Timeout: 10 * time.Second},
		started:        time.Now(),
		policy:         terminal.NewPolicySnapshot(cfg.BannedWords, cfg.CustomWords, cfg.DryRun, ""),
		policySource:   "local",
		heartbeatEvery: interval,
		done:           make(chan struct{}),
		counters: Counters{
			FindingsByKind: map[string]int64{},
		},
	}
	// The binary hash is the F20 baseline the heartbeat reports:
	// best effort — a sidecar that cannot read its own binary
	// reports an empty hash and the policy judges it accordingly.
	if exe, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(exe); err == nil {
			sum := sha256.Sum256(b)
			s.binaryHash = hex.EncodeToString(sum[:])
		}
	}
	return s, nil
}

// StartBackground loads the cached policy (if any) and starts the
// heartbeat loop. It is a no-op for the ticker when heartbeats are
// disabled; the policy cache still loads.
func (s *Server) StartBackground() {
	s.loadPolicyCache()
	if s.heartbeatEvery <= 0 {
		return
	}
	go func() {
		_ = s.sendHeartbeat() // best effort: usually no token yet
		t := time.NewTicker(s.heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				_ = s.sendHeartbeat()
			}
		}
	}()
}

// Stop ends the background heartbeat loop.
func (s *Server) Stop() {
	s.stopOnce.Do(func() { close(s.done) })
}

// ConfigHash is the SHA-256 of the sidecar's effective configuration
// (gateway URL + enforced word lists + dry-run + agent type +
// plugins), the value heartbeats report as config_hash so the F14
// policy can pin it. It changes when a policy refresh lands.
func (s *Server) ConfigHash() string {
	s.policyMu.Lock()
	snap := s.policy
	s.policyMu.Unlock()
	// DryRun is the sidecar's own rollout flag (from its local
	// config), not the gateway's: a policy refresh never flips
	// local enforcement on or off, so the hash uses cfg.DryRun.
	canonical, _ := json.Marshal(struct {
		GatewayURL  string   `json:"gateway_url"`
		BannedWords []string `json:"banned_words"`
		CustomWords []string `json:"custom_words"`
		DryRun      bool     `json:"dry_run"`
		AgentType   string   `json:"agent_type"`
		Plugins     []string `json:"plugins"`
	}{s.cfg.GatewayURL, snap.BannedWords, snap.CustomWords, s.cfg.DryRun, s.cfg.AgentType, s.cfg.Plugins})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// loadPolicyCache applies the on-disk policy snapshot, if it exists
// and verifies. A missing, corrupt or tampered cache is ignored:
// the sidecar keeps enforcing its startup word lists.
func (s *Server) loadPolicyCache() {
	if s.cfg.PolicyCachePath == "" {
		return
	}
	b, err := os.ReadFile(s.cfg.PolicyCachePath)
	if err != nil {
		return
	}
	var snap terminal.PolicySnapshot
	if err := json.Unmarshal(b, &snap); err != nil || !snap.Verify() {
		return
	}
	s.detMu.Lock()
	s.det = guard.NewDetector(snap.BannedWords, snap.CustomWords)
	s.detMu.Unlock()
	s.policyMu.Lock()
	s.policy = snap
	s.policySource = "cached"
	if fi, err := os.Stat(s.cfg.PolicyCachePath); err == nil {
		s.policyRefreshed = fi.ModTime()
	}
	s.policyMu.Unlock()
}

// savePolicyCache persists the current snapshot atomically (0600).
// Failures are ignored: the cache is an optimisation, and a failed
// write must not take down the proxy.
func (s *Server) savePolicyCache() {
	if s.cfg.PolicyCachePath == "" {
		return
	}
	s.policyMu.Lock()
	snap := s.policy
	s.policyMu.Unlock()
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	dir := filepath.Dir(s.cfg.PolicyCachePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp := s.cfg.PolicyCachePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.cfg.PolicyCachePath)
}

// refreshPolicy fetches the current server policy and, on success,
// swaps in the new detector and persists the cache. A failure keeps
// the current policy: the cached snapshot is the offline policy.
// The server's dry-run flag is NOT adopted: dry-run is the
// endpoint operator's rollout choice, and a refresh must never
// silently flip local enforcement on or off.
func (s *Server) refreshPolicy(token string) {
	req, err := http.NewRequest(http.MethodGet,
		strings.TrimSuffix(s.cfg.GatewayURL, "/")+"/api/sidecar/policy", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.shortCli.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var snap terminal.PolicySnapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&snap); err != nil || !snap.Verify() {
		return
	}
	s.detMu.Lock()
	s.det = guard.NewDetector(snap.BannedWords, snap.CustomWords)
	s.detMu.Unlock()
	s.policyMu.Lock()
	s.policy = snap
	s.policySource = "gateway"
	s.policyRefreshed = time.Now()
	s.policyMu.Unlock()
	s.savePolicyCache()
}

// maybeRefreshPolicy triggers a background refresh at most once per
// policyRefreshInterval. The token is the request's own bearer
// token: the sidecar never stores a credential of its own, it
// borrows the one the agent already presented.
func (s *Server) maybeRefreshPolicy(token string) {
	if token == "" {
		return
	}
	s.policyMu.Lock()
	stale := time.Since(s.policyRefreshed) >= policyRefreshInterval
	if stale {
		s.lastToken = token
	}
	s.policyMu.Unlock()
	if stale {
		go s.refreshPolicy(token)
	}
}

// offlineSummary builds the heartbeat backfill from the buffered
// outage decisions (counts and categories only).
func (s *Server) offlineSummary() *terminal.OfflineSummary {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if len(s.offline) == 0 {
		return nil
	}
	sum := &terminal.OfflineSummary{
		Events:      len(s.offline),
		WindowStart: s.offline[0].Time,
		WindowEnd:   s.offline[len(s.offline)-1].Time,
		ByCategory:  map[string]int64{},
	}
	for _, e := range s.offline {
		switch e.Decision {
		case "offline_blocked":
			sum.Blocked++
		case "offline_unreachable":
			sum.Unreachable++
		}
		for _, c := range e.Categories {
			sum.ByCategory[c]++
		}
	}
	return sum
}

// sendHeartbeat posts one terminal heartbeat (backlog F23). It uses
// the most recent bearer token seen on a proxied request; with no
// token yet there is nothing to authenticate as, so it waits. On
// success the offline buffer is cleared: the outage has been
// backfilled.
func (s *Server) sendHeartbeat() error {
	s.policyMu.Lock()
	token := s.lastToken
	s.policyMu.Unlock()
	if token == "" {
		return fmt.Errorf("no token seen yet")
	}
	agentType := s.cfg.AgentType
	if agentType == "" {
		agentType = "other"
	}
	hb := terminal.Heartbeat{
		AgentType:    agentType,
		Version:      Version,
		ConfigHash:   s.ConfigHash(),
		BinaryHash:   s.binaryHash,
		Plugins:      s.cfg.Plugins,
		GuardApplied: true,
		OS:           runtime.GOOS,
		Offline:      s.offlineSummary(),
	}
	body, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimSuffix(s.cfg.GatewayURL, "/")+"/api/terminals/heartbeat",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.shortCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("heartbeat: gateway returned %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	s.policyMu.Lock()
	s.offline = nil
	s.lastHeartbeat = time.Now()
	s.policyMu.Unlock()
	return nil
}

// Handler returns the daemon's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": SidecarHeader})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Status())
	})
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	return mux
}

// Status snapshots the daemon state. No mapping, token or matched
// text is ever included.
func (s *Server) Status() Status {
	s.policyMu.Lock()
	policyVersion := s.policy.Version
	policySource := s.policySource
	policyRefreshed := s.policyRefreshed
	lastHeartbeat := s.lastHeartbeat
	offlineBuffered := len(s.offline)
	heartbeatEnabled := s.heartbeatEvery > 0
	s.policyMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	kinds := make(map[string]int64, len(s.counters.FindingsByKind))
	for k, v := range s.counters.FindingsByKind {
		kinds[k] = v
	}
	recent := make([]Decision, len(s.recent))
	copy(recent, s.recent)
	st := Status{
		Version:    SidecarHeader,
		GatewayURL: s.cfg.GatewayURL,
		Listen:     s.cfg.Listen,
		DryRun:     s.cfg.DryRun,
		UptimeSecs: int64(time.Since(s.started).Seconds()),
		Counters: Counters{
			Requests:           s.counters.Requests,
			Forwarded:          s.counters.Forwarded,
			BlockedLocal:       s.counters.BlockedLocal,
			RedactedRequests:   s.counters.RedactedRequests,
			RestoredValues:     s.counters.RestoredValues,
			FindingsByKind:     kinds,
			OfflineBlocked:     s.counters.OfflineBlocked,
			OfflineUnreachable: s.counters.OfflineUnreachable,
		},
		Recent:           recent,
		PolicyVersion:    policyVersion,
		PolicySource:     policySource,
		HeartbeatEnabled: heartbeatEnabled,
		OfflineBuffered:  offlineBuffered,
	}
	if !policyRefreshed.IsZero() {
		t := policyRefreshed
		st.PolicyRefreshed = &t
	}
	if !lastHeartbeat.IsZero() {
		t := lastHeartbeat
		st.LastHeartbeat = &t
	}
	return st
}

// detector returns the current detector. Policy refreshes swap it
// under detMu; readers take the read lock.
func (s *Server) detector() *guard.Detector {
	s.detMu.RLock()
	defer s.detMu.RUnlock()
	return s.det
}

// policyAllowsOffline reports whether the cached policy lets a
// request at this level be treated as a retryable outage while the
// gateway is down (backlog F23 fail-closed rule).
func (s *Server) policyAllowsOffline(level store.DataLevel) bool {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	return s.policy.AllowsOffline(level.String())
}

// noteToken remembers the most recent bearer token for heartbeats
// and policy refreshes. Memory only, never written to disk.
func (s *Server) noteToken(token string) {
	if token == "" {
		return
	}
	s.policyMu.Lock()
	s.lastToken = token
	s.policyMu.Unlock()
}

// findingCategories lists the distinct "category:kind" pairs of a
// scan, sorted — the only finding detail any counter or decision
// ever carries.
func findingCategories(findings []guard.Finding) []string {
	kinds := map[string]bool{}
	for _, f := range findings {
		kinds[string(f.Category)+":"+f.Kind] = true
	}
	var cats []string
	for k := range kinds {
		cats = append(cats, k)
	}
	sort.Strings(cats)
	return cats
}

func (s *Server) record(d Decision, findings []guard.Finding, redacted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.Requests++
	for _, k := range findingCategories(findings) {
		s.counters.FindingsByKind[k]++
	}
	d.Categories = findingCategories(findings)
	s.recent = append([]Decision{d}, s.recent...)
	if len(s.recent) > maxRecentDecisions {
		s.recent = s.recent[:maxRecentDecisions]
	}
	if redacted > 0 {
		s.counters.RedactedRequests++
		s.counters.RestoredValues += int64(redacted)
	}
	switch d.Decision {
	case "block":
		s.counters.BlockedLocal++
	case "allow", "redact", "dry_run":
		s.counters.Forwarded++
	case "offline_blocked":
		s.counters.OfflineBlocked++
	case "offline_unreachable":
		s.counters.OfflineUnreachable++
	}
}

// recordOffline records a decision made while the gateway was
// unreachable and buffers it for the next heartbeat's backfill
// (backlog F23). The decision is counted exactly once, like any
// other local decision.
func (s *Server) recordOffline(d Decision, findings []guard.Finding, level store.DataLevel) {
	s.policyMu.Lock()
	s.offline = append(s.offline, offlineEntry{
		Time:       d.Time,
		Decision:   d.Decision,
		Categories: findingCategories(findings),
		Level:      level.String(),
	})
	s.policyMu.Unlock()
	s.record(d, findings, 0)
}

// handleModels passes the model list through to the gateway
// unchanged (same Authorization header the agent sent).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	up, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		strings.TrimSuffix(s.cfg.GatewayURL, "/")+"/v1/models", nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "bad_gateway", "cannot build upstream request")
		return
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		up.Header.Set("Authorization", auth)
	}
	resp, err := s.client.Do(up)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream_unavailable", "enterprise gateway unreachable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

// handleChatCompletions is the protection pipeline at the endpoint:
// scan -> local block on banned words -> redact -> forward to the
// gateway -> restore placeholders in the response.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "cannot read body")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid chat completion request")
		return
	}
	processed, findings, level, mapping, err := processBody(body, s.detector())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid chat completion request")
		return
	}
	// The request's bearer token is the sidecar's only credential:
	// heartbeats and policy refreshes authenticate with it (F23).
	// It is held in memory only and never written to disk.
	if token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); token != "" {
		s.noteToken(token)
		s.maybeRefreshPolicy(token)
	}
	dec := Decision{Time: time.Now(), Model: req.Model, DataLevel: level.String()}
	hasBanned := false
	for _, f := range findings {
		if f.Category == guard.CatBanned {
			hasBanned = true
		}
	}
	// Banned words never leave the machine. In dry-run mode the
	// request is only recorded, matching the gateway's DryRun.
	if hasBanned && !s.cfg.DryRun {
		dec.Decision = "block"
		s.record(dec, findings, 0)
		writeErr(w, http.StatusBadRequest, "sidecar_blocked",
			"request blocked by local privacy policy (banned content)")
		return
	}
	outBody := body
	redacted := 0
	if len(mapping) > 0 && !s.cfg.DryRun {
		outBody = processed
		redacted = len(mapping)
		dec.Decision = "redact"
	} else if s.cfg.DryRun && len(findings) > 0 {
		dec.Decision = "dry_run"
	} else {
		dec.Decision = "allow"
	}

	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimSuffix(s.cfg.GatewayURL, "/")+"/v1/chat/completions",
		bytes.NewReader(outBody))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "bad_gateway", "cannot build upstream request")
		return
	}
	up.Header.Set("Content-Type", "application/json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		up.Header.Set("Authorization", auth)
	}
	up.Header.Set("X-AgentGate-Sidecar", SidecarHeader)
	// The level is only ever raised, never lowered: the gateway
	// re-scans the (already redacted) text and takes the max.
	if level > store.LevelPublic {
		up.Header.Set("X-AgentGate-Data-Level", level.String())
	}
	if req.Stream {
		up.Header.Set("Accept", "text/event-stream")
	}
	resp, err := s.client.Do(up)
	if err != nil {
		// Gateway unreachable: enforce the cached policy offline
		// (backlog F23). Above the offline max level the call is
		// denied outright (fail-closed) and recorded as blocked;
		// below it the outage is retryable, recorded as
		// unreachable. Either way the decision is buffered for
		// the next heartbeat's backfill. Dry-run never blocks,
		// even offline.
		if !s.cfg.DryRun && !s.policyAllowsOffline(level) {
			dec.Decision = "offline_blocked"
			s.recordOffline(dec, findings, level)
			writeErr(w, http.StatusServiceUnavailable, "sidecar_offline_blocked",
				"request denied by cached offline policy: high data level while the enterprise gateway is unreachable")
			return
		}
		dec.Decision = "offline_unreachable"
		s.recordOffline(dec, findings, level)
		writeErr(w, http.StatusBadGateway, "upstream_unavailable", "enterprise gateway unreachable")
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "X-AgentGate-Cache"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// The decision is recorded once the outcome is known, so an
	// offline attempt is never also counted as forwarded.
	s.record(dec, findings, redacted)
	if req.Stream {
		// Streaming restore with the same holdback rule as the
		// gateway (F10): never emit the last MaxPlaceholderLength
		// bytes until the stream ends, so a placeholder split
		// across chunks is restored whole.
		_ = restoreStream(w, resp.Body, mapping)
		return
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if len(mapping) > 0 {
		respBody = []byte(guard.Restore(string(respBody), mapping))
	}
	_, _ = w.Write(respBody)
}

// restoreStream copies an SSE (or any) response body while restoring
// placeholders. It mirrors the gateway's streamCopy (F10 holdback
// pattern): the whole buffered remainder is restored on every read
// and only the prefix beyond MaxPlaceholderLength is emitted, so a
// placeholder split across reads is restored whole — restoring only
// the emitted prefix would cut a placeholder in half at the boundary
// and leak it. A partial placeholder stays in the pending tail until
// its remaining bytes arrive. With no mapping the body passes
// through untouched.
func restoreStream(w io.Writer, r io.Reader, mapping map[string]string) error {
	if len(mapping) == 0 {
		_, err := io.Copy(w, r)
		return err
	}
	hold := guard.MaxPlaceholderLength(mapping)
	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			restored := guard.Restore(string(pending), mapping)
			if len(restored) > hold {
				cut := len(restored) - hold
				// Never split a multi-byte UTF-8 rune at the cut.
				for cut > 0 && cut < len(restored) && !utf8.RuneStart(restored[cut]) {
					cut--
				}
				if _, err := io.WriteString(w, restored[:cut]); err != nil {
					return err
				}
				pending = append([]byte(nil), restored[cut:]...)
			} else {
				pending = []byte(restored)
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	if len(pending) > 0 {
		_, err := io.WriteString(w, guard.Restore(string(pending), mapping))
		return err
	}
	return nil
}

// chatRequest mirrors the gateway's request shape (model/stream/
// messages) for processing; unknown top-level fields are preserved
// in raw and re-emitted unchanged.
type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// processBody scans every message (content + tool_calls arguments)
// and returns the redacted body, all findings, the implied data
// level, and the placeholder mapping (empty when nothing was found).
// The mapping is the caller's to hold in memory for this request
// only; it must never be persisted or sent anywhere.
func processBody(body []byte, det *guard.Detector) (out []byte, findings []guard.Finding, level store.DataLevel, mapping map[string]string, err error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, nil, store.LevelPublic, nil, err
	}
	rawMsgs, ok := top["messages"]
	if !ok {
		return nil, nil, store.LevelPublic, nil, fmt.Errorf("missing messages")
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return nil, nil, store.LevelPublic, nil, err
	}
	red := guard.NewRedactor()
	for i, m := range msgs {
		if c, ok := m["content"]; ok {
			nc, fs := redactText(c, det, red)
			findings = append(findings, fs...)
			m["content"] = nc
		}
		if tc, ok := m["tool_calls"]; ok {
			ntc, fs := redactToolCalls(tc, det, red)
			findings = append(findings, fs...)
			m["tool_calls"] = ntc
		}
		msgs[i] = m
	}
	level = guard.Classify(findings)
	mapping = red.Mapping()
	if len(mapping) == 0 {
		return body, findings, level, mapping, nil
	}
	nb, err := json.Marshal(msgs)
	if err != nil {
		return nil, nil, store.LevelPublic, nil, err
	}
	top["messages"] = nb
	out, err = json.Marshal(top)
	if err != nil {
		return nil, nil, store.LevelPublic, nil, err
	}
	return out, findings, level, mapping, nil
}

// redactText redacts one content value: a plain string, or a parts
// array whose text parts are redacted in place (other part fields,
// e.g. image_url, are preserved untouched).
func redactText(raw json.RawMessage, det *guard.Detector, red *guard.Redactor) (json.RawMessage, []guard.Finding) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		fs := det.Scan(s)
		return json.RawMessage(mustJSON(red.Redact(s, fs))), fs
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		var all []guard.Finding
		for i, p := range parts {
			if t, ok := p["text"]; ok {
				var ts string
				if json.Unmarshal(t, &ts) == nil {
					fs := det.Scan(ts)
					all = append(all, fs...)
					p["text"] = json.RawMessage(mustJSON(red.Redact(ts, fs)))
					parts[i] = p
				}
			}
		}
		b, _ := json.Marshal(parts)
		return b, all
	}
	return raw, nil
}

// redactToolCalls redacts the function arguments string of every
// tool call (arguments are request payload, same as content — F10).
func redactToolCalls(raw json.RawMessage, det *guard.Detector, red *guard.Redactor) (json.RawMessage, []guard.Finding) {
	var calls []map[string]json.RawMessage
	if json.Unmarshal(raw, &calls) != nil {
		return raw, nil
	}
	var all []guard.Finding
	for i, c := range calls {
		fnRaw, ok := c["function"]
		if !ok {
			continue
		}
		var fn map[string]json.RawMessage
		if json.Unmarshal(fnRaw, &fn) != nil {
			continue
		}
		argsRaw, ok := fn["arguments"]
		if !ok {
			continue
		}
		var args string
		if json.Unmarshal(argsRaw, &args) != nil {
			continue
		}
		fs := det.Scan(args)
		all = append(all, fs...)
		fn["arguments"] = json.RawMessage(mustJSON(red.Redact(args, fs)))
		nf, _ := json.Marshal(fn)
		c["function"] = nf
		calls[i] = c
	}
	b, _ := json.Marshal(calls)
	return b, all
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}
