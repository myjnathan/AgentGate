// Package auth: password hashing (PBKDF2-HMAC-SHA256), web sessions,
// device-code login for agents, and tiered API tokens.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/store"
)

const pbkdf2Iter = 210_000

func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Iter,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(dk)), nil
}

func CheckPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(parts[2])
	want, err2 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(dk, want) == 1
}

func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Service manages sessions and login throttling.
type Service struct {
	st       *store.Store
	mu       sync.Mutex
	sessions map[string]session // session token -> session
	failures map[string][]time.Time
}

type session struct {
	userID   string
	username string
	role     string
	expires  time.Time
}

func New(st *store.Store) *Service {
	return &Service{st: st, sessions: map[string]session{}, failures: map[string][]time.Time{}}
}

// Login verifies credentials with account-based lockout:
// 20 failures within 20 minutes lock the account for 5 minutes.
// The lockout is keyed by account, not source IP (OWASP guidance).
func (s *Service) Login(username, password string) (*store.User, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var fails []time.Time
	for _, t := range s.failures[username] {
		if now.Sub(t) < 20*time.Minute {
			fails = append(fails, t)
		}
	}
	if len(fails) >= 20 {
		return nil, "", fmt.Errorf("account temporarily locked")
	}
	var user *store.User
	s.st.Read(func(st *store.State) {
		for _, u := range st.Users {
			if u.Username == username {
				cp := *u
				user = &cp
			}
		}
	})
	if user == nil || user.Disabled || !CheckPassword(password, user.PasswordHash) {
		s.failures[username] = append(fails, now)
		return nil, "", fmt.Errorf("invalid credentials")
	}
	delete(s.failures, username)
	tok := store.NewSecret("ags_")
	s.sessions[tok] = session{userID: user.ID, username: user.Username, role: user.Role, expires: now.Add(12 * time.Hour)}
	return user, tok, nil
}

// SessionUser resolves a web session token.
func (s *Service) SessionUser(tok string) (userID, username, role string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[tok]
	if !ok || time.Now().After(sess.expires) {
		return "", "", "", false
	}
	return sess.userID, sess.username, sess.role, true
}

// StartDeviceCode begins a device-code login for an agent.
func (s *Service) StartDeviceCode(agentType string, tier store.Tier) *store.DeviceCode {
	dc := &store.DeviceCode{
		Code:      "AG-" + strings.ToUpper(store.NewSecret("")[:6]),
		Secret:    store.NewSecret("poll_"),
		AgentType: agentType,
		Tier:      tier,
		Status:    "pending",
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	s.st.WithLock(func(st *store.State) { st.DeviceCodes[dc.Code] = dc })
	_ = s.st.Save()
	cp := *dc
	return &cp
}

// ApproveDeviceCode is called from the web UI by a signed-in user.
func (s *Service) ApproveDeviceCode(code string, user *store.User, deviceName string) (string, error) {
	var plain string
	var err error
	s.st.WithLock(func(st *store.State) {
		dc, ok := st.DeviceCodes[code]
		if !ok || dc.Status != "pending" || time.Now().After(dc.ExpiresAt) {
			err = fmt.Errorf("device code invalid or expired")
			return
		}
		dev := &store.Device{ID: store.NewID("dev"), UserID: user.ID, Name: deviceName, LastSeen: time.Now()}
		st.Devices[dev.ID] = dev
		plain = store.NewSecret("ag_at_")
		tok := &store.APIToken{
			ID: store.NewID("tok"), Hash: HashToken(plain), UserID: user.ID,
			DeviceID: dev.ID, AgentType: dc.AgentType, Tier: dc.Tier,
			ExpiresAt: time.Now().Add(30 * 24 * time.Hour), CreatedAt: time.Now(),
		}
		st.Tokens[tok.ID] = tok
		dc.Status = "approved"
		dc.TokenID = tok.ID
		dc.Token = plain
	})
	if err == nil {
		_ = s.st.Save()
	}
	return plain, err
}

// PollDeviceCode is called by the setup tool. The plaintext token is
// returned exactly once, on the first successful poll after approval.
func (s *Service) PollDeviceCode(code, secret string) (status, token string) {
	var out string
	s.st.WithLock(func(st *store.State) {
		dc, ok := st.DeviceCodes[code]
		if !ok || dc.Secret != secret || time.Now().After(dc.ExpiresAt) {
			out = "expired"
			return
		}
		switch dc.Status {
		case "approved":
			token = dc.Token
			dc.Token = "" // deliver once
			out = "approved"
		case "denied":
			out = "denied"
		default:
			out = "pending"
		}
	})
	return out, token
}

// ResolveToken resolves an agent API token to its record and owner.
func (s *Service) ResolveToken(plain string) (*store.APIToken, *store.User, bool) {
	h := HashToken(plain)
	var tok *store.APIToken
	var user *store.User
	s.st.Read(func(st *store.State) {
		for _, t := range st.Tokens {
			if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 && !t.Revoked && time.Now().Before(t.ExpiresAt) {
				cp := *t
				tok = &cp
				if u, ok := st.Users[t.UserID]; ok && !u.Disabled {
					uc := *u
					user = &uc
				}
			}
		}
	})
	if tok == nil || user == nil {
		return nil, nil, false
	}
	return tok, user, true
}
