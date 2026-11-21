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
)

// Tier is the trust tier of an agent installation / token.
type Tier string

const (
	TierManaged Tier = "managed" // 受管级: passed admission, may read encrypted files
	TierBYOA    Tier = "byoa"    // 自带级: employee's own agent, general capabilities only
)

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
}

// Channel is an upstream model provider. APIKey is stored encrypted
// (AES-GCM, see package secretbox); KeyMasked is safe to display.
type Channel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	BaseURL   string   `json:"base_url"`
	APIKeyEnc string   `json:"-"`
	KeyMasked string   `json:"key_masked"`
	Models    []string `json:"models"`
	// MaxDataLevel is the highest data level this channel may receive.
	// Public-cloud channels are typically LevelInternal; private
	// deployments may be LevelSecret.
	MaxDataLevel DataLevel `json:"max_data_level"`
	Weight       int       `json:"weight"`
	Disabled     bool      `json:"disabled"`
	Failures     int       `json:"-"`
	// ConsecutiveFailures counts retryable failures since the last
	// success; reaching the gateway's fail threshold cools the channel
	// down (see internal/gateway). Runtime-only, never persisted.
	ConsecutiveFailures int `json:"-"`
	// CooldownUntil is set when the channel enters cooldown; while
	// time.Now() is before it, the gateway never picks this channel.
	CooldownUntil time.Time `json:"-"`
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

type State struct {
	Users       map[string]*User       `json:"users"`
	Devices     map[string]*Device     `json:"devices"`
	Tokens      map[string]*APIToken   `json:"tokens"` // by ID
	DeviceCodes map[string]*DeviceCode `json:"device_codes"`
	Channels    map[string]*Channel    `json:"channels"`
	Skills      map[string]*Skill      `json:"skills"`
	KBDocs      map[string]*KBDoc      `json:"kb_docs"`
}

func newState() *State {
	return &State{
		Users:       map[string]*User{},
		Devices:     map[string]*Device{},
		Tokens:      map[string]*APIToken{},
		DeviceCodes: map[string]*DeviceCode{},
		Channels:    map[string]*Channel{},
		Skills:      map[string]*Skill{},
		KBDocs:      map[string]*KBDoc{},
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
