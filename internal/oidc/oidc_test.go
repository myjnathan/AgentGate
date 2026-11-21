package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeIdP is a minimal OpenID Connect provider for tests: discovery,
// JWKS, token endpoint (authorization-code + PKCE) and userinfo. Its
// ID tokens carry only sub (like the Duende demo, which puts profile
// claims in userinfo instead of the ID token).
type fakeIdP struct {
	server  *httptest.Server
	key     *rsa.PrivateKey
	kid     string
	issuer  string
	nonceOf func() string // returns the nonce the token must carry
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, kid: "test-kid-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 f.issuer,
			"authorization_endpoint": f.issuer + "/authorize",
			"token_endpoint":         f.issuer + "/token",
			"userinfo_endpoint":      f.issuer + "/userinfo",
			"jwks_uri":               f.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(f.key.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.PublicKey.E)).Bytes())
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{
			{"kid": f.kid, "kty": "RSA", "use": "sig", "n": n, "e": e},
		}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "test-code" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		// PKCE: a verifier must be presented (the challenge was sent
		// at the authorize step; the real IdP checks it matches).
		if r.Form.Get("code_verifier") == "" {
			http.Error(w, "missing verifier", http.StatusBadRequest)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "test-client" || pass != "test-secret" {
			http.Error(w, "bad client auth", http.StatusUnauthorized)
			return
		}
		now := time.Now()
		claims := map[string]any{
			"iss": f.issuer, "aud": "test-client", "sub": "user-123",
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			"nonce": f.nonceOf(),
		}
		idToken, err := signJWT(f.key, f.kid, claims)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id_token": idToken, "access_token": "test-access-token",
			"token_type": "Bearer", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": "user-123", "name": "Alice Demo", "preferred_username": "alice",
			"email": "alice@example.test", "department": "研发部",
		})
	})
	f.server = httptest.NewServer(mux)
	f.issuer = f.server.URL
	return f
}

func signJWT(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	input := enc(header) + "." + enc(payload)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + enc(sig), nil
}

func newTestProvider(t *testing.T, f *fakeIdP) *Provider {
	t.Helper()
	cfg := Config{Issuer: f.issuer, ClientID: "test-client", ClientSecret: "test-secret",
		RedirectURL: "http://127.0.0.1:8787/auth/oidc/callback", Scopes: "openid profile email"}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if _, ok := ConfigFromEnv(get); ok {
		t.Fatal("empty env must not enable OIDC")
	}
	env["AGENTGATE_OIDC_ISSUER"] = "https://demo.duendesoftware.com"
	env["AGENTGATE_OIDC_CLIENT_ID"] = "interactive.confidential"
	env["AGENTGATE_OIDC_CLIENT_SECRET"] = "secret"
	cfg, ok := ConfigFromEnv(get)
	if !ok {
		t.Fatal("full env must enable OIDC")
	}
	if cfg.RedirectURL != "http://127.0.0.1:8787/auth/oidc/callback" || cfg.Scopes != "openid profile email" || cfg.DepartmentClaim != "department" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	env["AGENTGATE_OIDC_ISSUER"] = "http://insecure.example.test"
	cfg, _ = ConfigFromEnv(get)
	if err := cfg.Validate(); err == nil {
		t.Fatal("http issuer must be rejected")
	}
}

func TestFullExchangeFlow(t *testing.T) {
	f := newFakeIdP(t)
	defer f.server.Close()
	p := newTestProvider(t, f)
	// The fake IdP mints the nonce the pending state holds; capture it
	// now — Exchange deletes the state (single-use) before calling
	// the token endpoint.
	var wantNonce string
	{
		authURL, err := p.AuthURL()
		if err != nil {
			t.Fatalf("AuthURL: %v", err)
		}
		u, err := url.Parse(authURL)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if q.Get("client_id") != "test-client" || q.Get("response_type") != "code" ||
			q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
			q.Get("nonce") == "" || q.Get("state") == "" {
			t.Fatalf("auth URL missing parameters: %s", authURL)
		}
		if !strings.HasPrefix(authURL, f.issuer+"/authorize?") {
			t.Fatalf("auth URL must start at the discovery authorization endpoint: %s", authURL)
		}
		p.mu.Lock()
		wantNonce = p.states[q.Get("state")].nonce
		p.mu.Unlock()
		f.nonceOf = func() string { return wantNonce }
		claims, err := p.Exchange(context.Background(), "test-code", q.Get("state"))
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		if claims.Subject != "user-123" {
			t.Fatalf("sub = %q", claims.Subject)
		}
		// Profile claims come from userinfo (the ID token has none).
		if claims.Name != "Alice Demo" || claims.PreferredUsername != "alice" ||
			claims.Email != "alice@example.test" || claims.Department != "研发部" {
			t.Fatalf("userinfo merge failed: %+v", claims)
		}
		// State is single-use.
		if _, err := p.Exchange(context.Background(), "test-code", q.Get("state")); err == nil {
			t.Fatal("reused state must be rejected")
		}
	}
}

func TestExchangeRejectsBadCode(t *testing.T) {
	f := newFakeIdP(t)
	defer f.server.Close()
	f.nonceOf = func() string { return "x" }
	p := newTestProvider(t, f)
	authURL, _ := p.AuthURL()
	u, _ := url.Parse(authURL)
	if _, err := p.Exchange(context.Background(), "wrong-code", u.Query().Get("state")); err == nil {
		t.Fatal("wrong code must be rejected")
	}
}

func mintToken(t *testing.T, f *fakeIdP, mutate func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss": f.issuer, "aud": "test-client", "sub": "user-123",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": "test-nonce",
	}
	if mutate != nil {
		mutate(claims)
	}
	tok, err := signJWT(f.key, f.kid, claims)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestVerifyIDToken(t *testing.T) {
	f := newFakeIdP(t)
	defer f.server.Close()
	p := newTestProvider(t, f)
	ctx := context.Background()

	if _, err := p.VerifyIDToken(ctx, mintToken(t, f, nil), "test-nonce"); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		token  func() string
	}{
		{"wrong nonce", func(c map[string]any) { c["nonce"] = "other" }, nil},
		{"wrong audience", func(c map[string]any) { c["aud"] = "other-client" }, nil},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example.test" }, nil},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, nil},
		{"no subject", func(c map[string]any) { delete(c, "sub") }, nil},
		{"tampered payload", nil, func() string {
			tok := mintToken(t, f, nil)
			parts := strings.Split(tok, ".")
			// Flip a bit in the payload without re-signing.
			raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
			raw[10] ^= 1
			parts[1] = base64.RawURLEncoding.EncodeToString(raw)
			return strings.Join(parts, ".")
		}},
		{"unsigned alg none", nil, func() string {
			header, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
			payload, _ := json.Marshal(map[string]any{"iss": f.issuer, "aud": "test-client", "sub": "user-123"})
			enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
			return enc(header) + "." + enc(payload) + "."
		}},
		{"unknown kid", nil, func() string {
			other, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			tok, err := signJWT(other, "unknown-kid", map[string]any{
				"iss": f.issuer, "aud": "test-client", "sub": "user-123",
				"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": "test-nonce",
			})
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := ""
			if tc.token != nil {
				tok = tc.token()
			} else {
				tok = mintToken(t, f, tc.mutate)
			}
			if _, err := p.VerifyIDToken(ctx, tok, "test-nonce"); err == nil {
				t.Fatalf("expected rejection: %s", tc.name)
			}
		})
	}
}

func TestDiscoveryIssuerMismatch(t *testing.T) {
	f := newFakeIdP(t)
	defer f.server.Close()
	cfg := Config{Issuer: "https://other.example.test", ClientID: "test-client",
		ClientSecret: "test-secret", RedirectURL: "http://127.0.0.1:8787/auth/oidc/callback",
		Scopes: "openid profile email"}
	if _, err := NewProvider(cfg); err == nil {
		t.Fatal("discovery issuer mismatch must be rejected")
	}
}
