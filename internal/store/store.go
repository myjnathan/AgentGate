// Package store holds AgentGate's data model and persistence.
//
// Night-one implementation: an in-memory store with JSON snapshot
// persistence in the data directory. The entity model follows the
// blueprint (docs/blueprint.md §7); the storage engine is scheduled to
// move to SQL (SQLite/PostgreSQL/MySQL) behind this same API.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/attest"
)

// Tier is the trust tier of an agent installation / token.
type Tier string

const (
	TierManaged Tier = "managed" // 受管级: passed admission, may read encrypted files
	TierBYOA    Tier = "byoa"    // 自带级: employee's own agent, general capabilities only
)

// TrustType is the trust tier of a model channel's deployment: where the
// data physically goes and what the provider promises. Routing binds to
// the trust type, not to whether the enterprise owns a model (F17):
// most companies have no model of their own, no GPU and no ops team, so
// "private model" cannot be the default. Gradient:
//   - signed_cloud: contracted cloud model (zero-retention / no-training
//     commitment) — public/internal data, and confidential data only
//     after gateway desensitization.
//   - dedicated_cloud: vendor dedicated cloud / VPC deployment —
//     confidential data as-is (optional upgrade path).
//   - local_model: small on-device model — the only trust type that may
//     receive top-secret data; otherwise the request is blocked with an
//     explicit "no compliant channel" reason.
type TrustType string

const (
	TrustSignedCloud    TrustType = "signed_cloud"    // 签约云
	TrustDedicatedCloud TrustType = "dedicated_cloud" // 专有云 / VPC
	TrustLocalModel     TrustType = "local_model"     // 本地小模型
)

func (t TrustType) String() string { return string(t) }

func (t TrustType) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(t))
}

func (t *TrustType) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*t = TrustSignedCloud
		return nil
	}
	v, ok := ParseTrustType(s)
	if !ok {
		return fmt.Errorf("invalid trust type %q", s)
	}
	*t = v
	return nil
}

// ParseTrustType parses "signed_cloud|dedicated_cloud|local_model".
func ParseTrustType(s string) (TrustType, bool) {
	switch s {
	case "signed_cloud":
		return TrustSignedCloud, true
	case "dedicated_cloud":
		return TrustDedicatedCloud, true
	case "local_model":
		return TrustLocalModel, true
	}
	return "", false
}

// DataLevel is the classification of content flowing through the gateway.
type DataLevel int

const (
	LevelPublic       DataLevel = 0 // 公开
	LevelInternal     DataLevel = 1 // 内部
	LevelConfidential DataLevel = 2 // 机密
	LevelSecret       DataLevel = 3 // 绝密
)

func (l DataLevel) String() string {
	switch l {
	case LevelPublic:
		return "public"
	case LevelInternal:
		return "internal"
	case LevelConfidential:
		return "confidential"
	case LevelSecret:
		return "secret"
	}
	return "unknown"
}

func (l DataLevel) MarshalJSON() ([]byte, error) {
	return json.Marshal(l.String())
}

func (l *DataLevel) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// tolerate numeric form from older snapshots
		var n int
		if err2 := json.Unmarshal(b, &n); err2 == nil {
			*l = DataLevel(n)
			return nil
		}
		return err
	}
	v, ok := ParseDataLevel(s)
	if !ok {
		return fmt.Errorf("invalid data level %q", s)
	}
	*l = v
	return nil
}

// ParseDataLevel parses "public|internal|confidential|secret".
func ParseDataLevel(s string) (DataLevel, bool) {
	switch s {
	case "public":
		return LevelPublic, true
	case "internal":
		return LevelInternal, true
	case "confidential":
		return LevelConfidential, true
	case "secret":
		return LevelSecret, true
	}
	return LevelPublic, false
}

type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name"`
	PasswordHash string `json:"password_hash"`
	Department   string `json:"department"`
	Role         string `json:"role"` // admin | security | operator | employee
	Disabled     bool   `json:"disabled"`
	// OIDCSubject is the IdP "sub" for users provisioned via OIDC
	// (backlog F9); empty for local accounts. AuthSource is
	// "local" or "oidc" and records how the account was created.
	OIDCSubject string `json:"oidc_subject,omitempty"`
	AuthSource  string `json:"auth_source,omitempty"`
}

type Device struct {
	ID       string    `json:"id"`
	UserID   string    `json:"user_id"`
	Name     string    `json:"name"`
	LastSeen time.Time `json:"last_seen"`
}

// APIToken is a tiered token issued to an agent installation.
// Only the SHA-256 hash of the token is stored.
type APIToken struct {
	ID         string    `json:"id"`
	Hash       string    `json:"hash"`
	UserID     string    `json:"user_id"`
	DeviceID   string    `json:"device_id"`
	AgentType  string    `json:"agent_type"` // dsh | claude-code | codex | workbuddy | other
	Tier       Tier      `json:"tier"`
	ExpiresAt  time.Time `json:"expires_at"`
	Revoked    bool      `json:"revoked"`
	CreatedAt  time.Time `json:"created_at"`
	MonthlyCap int64     `json:"monthly_cap_tokens"` // 0 = unlimited
	UsedTokens int64     `json:"used_tokens"`
	// RPM/TPM are the per-minute rate limits for this token
	// (backlog F3). 0 = fall back to the tier default.
	RPM int `json:"rpm_limit"`
	TPM int `json:"tpm_limit"`
	// Attested records whether this token was issued after a passing
	// agent attestation (backlog F14); AttestVerdict is the
	// attestation result at issuance ("pass" | "downgraded" |
	// "unverified"). A managed-tier token with Attested=false was
	// issued without an applicable policy or without a required
	// report (unverified), never after a failed attestation — a
	// failed attestation is downgraded to BYOA instead.
	Attested      bool   `json:"attested"`
	AttestVerdict string `json:"attest_verdict,omitempty"`
}

// Webhook is an alert delivery target (backlog F5). SecretEnc holds
// the optional HMAC signing secret encrypted with the master key
// (AES-GCM, see package secretbox); it is persisted (unlike
// Channel.Keys, which predates persistence) so admin-configured
// webhooks survive restarts. The plaintext secret and the full URL
// query (which may carry a bot key) are never returned by the API.
type Webhook struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Kind      string    `json:"kind"` // generic | wecom | dingtalk | feishu
	SecretEnc string    `json:"secret_enc,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// UpstreamServer is a federated upstream MCP server (backlog F6).
// Its tools are exposed through AgentGate's own /mcp endpoint under
// the "<name>.<tool>" namespace. AuthTokenEnc holds the optional
// bearer token for the upstream, encrypted with the master key
// (AES-GCM, see package secretbox); it is never returned by the API.
// MaxDataLevel caps the data level that may be sent to this upstream;
// MinTier is the lowest token tier allowed to call its tools.
type UpstreamServer struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"` // namespace, e.g. "hr"
	URL          string    `json:"url"`
	AuthTokenEnc string    `json:"-"`
	HasToken     bool      `json:"has_token"`
	MaxDataLevel DataLevel `json:"max_data_level"`
	MinTier      Tier      `json:"min_tier"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
}

// RESTTool is a registered REST endpoint exposed as an MCP tool
// (backlog F7). It appears in /mcp tools/list as "rest.<name>" and a
// call is translated into one HTTP request: path template parameters
// ({{param}}) are substituted and URL-escaped, the remaining
// arguments go to the query string (GET/DELETE) or a JSON body
// (POST/PUT/PATCH). AuthTokenEnc holds the optional hosted credential,
// injected as an Authorization header and decrypted only at call
// time; it is never returned by the API. InputSchema is the JSON
// Schema (object) the arguments are validated against before any
// request is built. MaxDataLevel caps the data level that may be sent
// to this endpoint; MinTier is the lowest token tier allowed to call it.
type RESTTool struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	BaseURL      string            `json:"base_url"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	Headers      map[string]string `json:"headers,omitempty"`
	AuthTokenEnc string            `json:"-"`
	HasToken     bool              `json:"has_token"`
	InputSchema  json.RawMessage   `json:"input_schema"`
	MaxDataLevel DataLevel         `json:"max_data_level"`
	MinTier      Tier              `json:"min_tier"`
	Enabled      bool              `json:"enabled"`
	CreatedAt    time.Time         `json:"created_at"`
}

type DeviceCode struct {
	Code      string    `json:"code"`   // user-facing code, e.g. AG-XXXXXX
	Secret    string    `json:"secret"` // polling secret held by the setup tool
	AgentType string    `json:"agent_type"`
	Tier      Tier      `json:"tier"`
	Status    string    `json:"status"` // pending | approved | denied
	TokenID   string    `json:"token_id,omitempty"`
	Token     string    `json:"-"` // plaintext token, delivered once on poll after approval
	ExpiresAt time.Time `json:"expires_at"`
	// Attestation is the evidence the agent submitted with this code
	// (backlog F14); AttestVerdict/AttestReasons are the evaluation
	// result at submission time (re-evaluated against the current
	// policy at approval). Empty verdict means no report was ever
	// submitted for this code.
	Attestation   *attest.Report `json:"attestation,omitempty"`
	AttestVerdict string         `json:"attest_verdict,omitempty"`
	AttestReasons []string       `json:"attest_reasons,omitempty"`
}

// ChannelKey is one upstream API key held by a channel (backlog F12).
// Keys are addressed by stable index (0 = the key the channel was
// created with) — the same indexing new-api uses for its per-key status
// list (QuantumNous/new-api, AGPL-3.0, model/channel.go; reused under
// Nathan's 2026-11-21 reuse-first instruction, AGPL-3.0 is compatible
// with this project's AGPL-3.0). Enc holds the key AES-GCM-encrypted
// with the master key (see package secretbox); it is never persisted
// (json:"-") and never returned by the API — only Masked (last 4
// chars) is ever displayed. Disabled marks a key the gateway removed
// after the upstream rejected it with 401, or an admin disabled by
// hand; DisabledReason/DisabledAt record why and when. Like channel
// health (F1), key state is runtime-only: a restart re-enables keys
// and the gateway re-proves them against the upstream.
type ChannelKey struct {
	Enc            string    `json:"-"`
	Masked         string    `json:"masked"`
	Disabled       bool      `json:"disabled"`
	DisabledReason string    `json:"disabled_reason,omitempty"`
	DisabledAt     time.Time `json:"disabled_at,omitempty"`
}

// ModelPrice is the per-model price of one channel, in USD per
// million tokens (backlog F13). The shape follows LiteLLM's model
// cost table (input_cost_per_token / output_cost_per_token, scaled
// to per-million for readability): a model may cost differently to
// prompt and to complete, and the price is configured per channel
// because the same model can be bought at different rates from
// different providers. No price means the model is unpriced —
// calls are still served and audited, but stats report them as
// unpriced instead of inventing a cost.
type ModelPrice struct {
	InputPerMillion  float64 `json:"input_per_million"`
	OutputPerMillion float64 `json:"output_per_million"`
}

// DepartmentBudget is a monthly spend cap for one department
// (backlog F13). The month is the calendar month in UTC and resets
// on the 1st at 00:00 UTC (Portkey's monthly reset). MonthlyCapUSD
// is the hard cap: once the department's spend this month reaches
// it, the gateway rejects further model calls with 429
// department_budget_exceeded until the next month. AlertThreshold,
// when > 0, is the fraction of the cap (e.g. 0.8) at which one
// warning audit event (and, via F5, one alert) fires per month.
type DepartmentBudget struct {
	ID             string    `json:"id"`
	Department     string    `json:"department"`
	MonthlyCapUSD  float64   `json:"monthly_cap_usd"`
	AlertThreshold float64   `json:"alert_threshold"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Channel is an upstream model provider. Keys holds every upstream API
// key for this channel (index 0 is the primary key); the gateway
// round-robins across the enabled keys and auto-disables any key the
// upstream rejects with 401 (F12). At most MaxChannelKeys keys.
type Channel struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	BaseURL string       `json:"base_url"`
	Keys    []ChannelKey `json:"-"`
	Models  []string     `json:"models"`
	// Pricing maps model name -> price per million tokens (F13).
	// A model without an entry is unpriced.
	Pricing map[string]ModelPrice `json:"pricing,omitempty"`
	// MaxDataLevel is the highest data level this channel may receive.
	// Public-cloud channels are typically LevelInternal; private
	// deployments may be LevelSecret.
	MaxDataLevel DataLevel `json:"max_data_level"`
	// TrustType is where the data goes and what the deployment promises
	// (signed_cloud | dedicated_cloud | local_model, F17). Empty in
	// snapshots written before F17 means signed_cloud.
	TrustType TrustType `json:"trust_type"`
	Weight    int       `json:"weight"`
	Disabled  bool      `json:"disabled"`
	Failures  int       `json:"-"`
	// ConsecutiveFailures counts retryable failures since the last
	// success; reaching the gateway's fail threshold cools the channel
	// down (see internal/gateway). Runtime-only, never persisted.
	ConsecutiveFailures int `json:"-"`
	// CooldownUntil is set when the channel enters cooldown; while
	// time.Now() is before it, the gateway never picks this channel.
	CooldownUntil time.Time `json:"-"`
}

// MaxChannelKeys caps the upstream API keys one channel may hold
// (F12): enough for a rotation set, small enough to keep the 401
// retry loop inside one request bounded.
const MaxChannelKeys = 10

// KeyCount returns the number of upstream keys on the channel.
func (c *Channel) KeyCount() int { return len(c.Keys) }

// EnabledKeyCount returns how many of the channel's keys are enabled.
func (c *Channel) EnabledKeyCount() int {
	n := 0
	for _, k := range c.Keys {
		if !k.Disabled {
			n++
		}
	}
	return n
}

// PrimaryMasked returns the masked form of the first key ("" when the
// channel has no key yet); it is what the management API displays.
func (c *Channel) PrimaryMasked() string {
	if len(c.Keys) == 0 {
		return ""
	}
	return c.Keys[0].Masked
}

// TrustTypeOrDefault returns the channel's trust type; channels stored
// before F17 have none and are treated as signed cloud (the previous
// default: a public-cloud channel capped at internal data).
func (c *Channel) TrustTypeOrDefault() TrustType {
	if c.TrustType == "" {
		return TrustSignedCloud
	}
	return c.TrustType
}

// Health reports the runtime health of the channel:
// "disabled" | "cooling_down" | "healthy".
func (c *Channel) Health(now time.Time) string {
	if c.Disabled {
		return "disabled"
	}
	if !c.CooldownUntil.IsZero() && now.Before(c.CooldownUntil) {
		return "cooling_down"
	}
	return "healthy"
}

type Skill struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Content   string    `json:"content"` // SKILL.md body
	Scope     string    `json:"scope"`   // comma-separated departments, or "*"
	Status    string    `json:"status"`  // draft | in_review | published | rejected
	AuthorID  string    `json:"author_id"`
	Checksum  string    `json:"checksum"`
	UpdatedAt time.Time `json:"updated_at"`
}

type KBDoc struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Content    string    `json:"content"`
	Department string    `json:"department"`
	Level      DataLevel `json:"level"`
	Version    int       `json:"version"`
}

// KBFolderSource maps a shared folder to a data level (backlog F18):
// every supported file under Path is indexed into the knowledge base
// at this level. The index itself lives in memory (internal/folderkb);
// only the registration is persisted here.
type KBFolderSource struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Level     DataLevel `json:"level"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

type State struct {
	Users       map[string]*User           `json:"users"`
	Devices     map[string]*Device         `json:"devices"`
	Tokens      map[string]*APIToken       `json:"tokens"` // by ID
	DeviceCodes map[string]*DeviceCode     `json:"device_codes"`
	Channels    map[string]*Channel        `json:"channels"`
	Skills      map[string]*Skill          `json:"skills"`
	KBDocs      map[string]*KBDoc          `json:"kb_docs"`
	Webhooks    map[string]*Webhook        `json:"webhooks"`
	Upstreams   map[string]*UpstreamServer `json:"upstreams"`
	RESTTools   map[string]*RESTTool       `json:"rest_tools"`
	Prompts     map[string]*PromptTemplate `json:"prompts"`
	// Budgets holds the monthly department spend caps (backlog F13),
	// keyed by budget ID; at most one budget per department.
	Budgets map[string]*DepartmentBudget `json:"budgets"`
	// KBFolderSources registers shared folders as knowledge-base
	// sources (backlog F18); the file index is rebuilt in memory.
	KBFolderSources map[string]*KBFolderSource `json:"kb_folder_sources"`
	// AttestationPolicies holds the appraisal policy per agent type
	// (backlog F14), keyed by agent type; an agent type with no entry
	// has no attestation requirement.
	AttestationPolicies map[string]*attest.Policy `json:"attestation_policies"`
}

func newState() *State {
	return &State{
		Users:               map[string]*User{},
		Devices:             map[string]*Device{},
		Tokens:              map[string]*APIToken{},
		DeviceCodes:         map[string]*DeviceCode{},
		Channels:            map[string]*Channel{},
		Skills:              map[string]*Skill{},
		KBDocs:              map[string]*KBDoc{},
		Webhooks:            map[string]*Webhook{},
		Upstreams:           map[string]*UpstreamServer{},
		RESTTools:           map[string]*RESTTool{},
		Prompts:             map[string]*PromptTemplate{},
		Budgets:             map[string]*DepartmentBudget{},
		KBFolderSources:     map[string]*KBFolderSource{},
		AttestationPolicies: map[string]*attest.Policy{},
	}
}

// Store is a concurrency-safe store with JSON snapshot persistence.
type Store struct {
	mu   sync.RWMutex
	path string
	st   *State
}

func New(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "state.json"), st: newState()}
	if b, err := os.ReadFile(s.path); err == nil {
		var st State
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, fmt.Errorf("load state: %w", err)
		}
		// Fill any maps added in newer versions.
		if st.Users == nil {
			st.Users = map[string]*User{}
		}
		if st.Devices == nil {
			st.Devices = map[string]*Device{}
		}
		if st.Tokens == nil {
			st.Tokens = map[string]*APIToken{}
		}
		if st.DeviceCodes == nil {
			st.DeviceCodes = map[string]*DeviceCode{}
		}
		if st.Channels == nil {
			st.Channels = map[string]*Channel{}
		}
		if st.Skills == nil {
			st.Skills = map[string]*Skill{}
		}
		if st.KBDocs == nil {
			st.KBDocs = map[string]*KBDoc{}
		}
		if st.Webhooks == nil {
			st.Webhooks = map[string]*Webhook{}
		}
		if st.Upstreams == nil {
			st.Upstreams = map[string]*UpstreamServer{}
		}
		if st.RESTTools == nil {
			st.RESTTools = map[string]*RESTTool{}
		}
		if st.Prompts == nil {
			st.Prompts = map[string]*PromptTemplate{}
		}
		if st.Budgets == nil {
			st.Budgets = map[string]*DepartmentBudget{}
		}
		if st.KBFolderSources == nil {
			st.KBFolderSources = map[string]*KBFolderSource{}
		}
		s.st = &st
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// Save persists a snapshot. Callers invoke it after mutations.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// WithLock runs fn with exclusive access to the state.
func (s *Store) WithLock(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.st)
}

// Read runs fn with shared access to the state.
func (s *Store) Read(fn func(*State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.st)
}

// NewID returns a random hex ID with a prefix.
func NewID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// NewSecret returns a random token string with a prefix.
func NewSecret(prefix string) string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
