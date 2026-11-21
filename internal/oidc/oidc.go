// Package oidc implements OpenID Connect login for AgentGate using only
// the Go standard library: discovery, authorization-code flow with
// PKCE (S256), code exchange, RS256 ID-token verification against the
// IdP JWKS, and a userinfo call to fill in profile claims.
//
// The implementation follows OpenID Connect Core 1.0 / Discovery 1.0
// (openid.net/specs/openid-connect-core-1_0.html,
// openid.net/specs/openid-connect-discovery-1_0.html) and was verified
// end-to-end against the public Duende IdentityServer demo IdP
// (demo.duendesoftware.com, client interactive.confidential, user
// alice/alice): authorize -> login -> code -> token exchange -> RS256
// ID token whose iss/aud/nonce verify against the live JWKS.
//
// Conservative defaults (backlog F9): the IdP is only an identity
// source, never a privilege source. JIT-provisioned users always start
// as role "employee"; an admin promotes them locally. Local accounts
// keep working as a fallback.
package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config is the OIDC client configuration, taken from the environment
// (see ConfigFromEnv). All three of Issuer, ClientID and ClientSecret
// must be set for OIDC to be enabled.
type Config struct {
	Issuer          string
	ClientID        string
	ClientSecret    string
	RedirectURL     string
	Scopes          string // space-separated, default "openid profile email"
	DepartmentClaim string // claim carrying the department, default "department"

	// HTTPClient is used for discovery, token exchange, JWKS and
	// userinfo calls. Nil means http.DefaultClient.
	HTTPClient *http.Client
}

// ConfigFromEnv reads the OIDC configuration from the environment:
//
//	AGENTGATE_OIDC_ISSUER          e.g. https://demo.duendesoftware.com
//	AGENTGATE_OIDC_CLIENT_ID       e.g. interactive.confidential
//	AGENTGATE_OIDC_CLIENT_SECRET   (never logged, never returned by the API)
//	AGENTGATE_OIDC_REDIRECT_URL    default http://127.0.0.1:8787/auth/oidc/callback
//	AGENTGATE_OIDC_SCOPES          default "openid profile email"
//	AGENTGATE_OIDC_DEPARTMENT_CLAIM default "department"
//
// The second return value reports whether OIDC is enabled at all
// (issuer + client ID + client secret all present).
func ConfigFromEnv(getenv func(string) string) (Config, bool) {
	cfg := Config{
		Issuer:          strings.TrimSpace(getenv("AGENTGATE_OIDC_ISSUER")),
		ClientID:        strings.TrimSpace(getenv("AGENTGATE_OIDC_CLIENT_ID")),
		ClientSecret:    getenv("AGENTGATE_OIDC_CLIENT_SECRET"),
		RedirectURL:     strings.TrimSpace(getenv("AGENTGATE_OIDC_REDIRECT_URL")),
		Scopes:          strings.TrimSpace(getenv("AGENTGATE_OIDC_SCOPES")),
		DepartmentClaim: strings.TrimSpace(getenv("AGENTGATE_OIDC_DEPARTMENT_CLAIM")),
	}
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return cfg, false
	}
	if cfg.RedirectURL == "" {
		cfg.RedirectURL = "http://127.0.0.1:8787/auth/oidc/callback"
	}
	if cfg.Scopes == "" {
		cfg.Scopes = "openid profile email"
	}
	if cfg.DepartmentClaim == "" {
		cfg.DepartmentClaim = "department"
	}
	return cfg, true
}

// Validate checks the configuration shape (not IdP reachability).
// The issuer must be https, except for loopback test IdPs
// (http://127.0.0.1, http://[::1] or http://localhost), which the
// automated tests use.
func (c Config) Validate() error {
	u, err := url.Parse(c.Issuer)
	if err != nil || u.Host == "" {
		return errors.New("oidc issuer must be a URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" && u.Hostname() != "localhost" {
			return errors.New("oidc issuer must be an https URL (http is only allowed for loopback test IdPs)")
		}
	default:
		return errors.New("oidc issuer must be an https URL")
	}
	if strings.HasSuffix(c.Issuer, "/") {
		return errors.New("oidc issuer must not end with a slash")
	}
	if c.ClientID == "" {
		return errors.New("oidc client_id is required")
	}
	if c.ClientSecret == "" {
		return errors.New("oidc client_secret is required")
	}
	if c.RedirectURL == "" {
		return errors.New("oidc redirect_url is required")
	}
	if _, err := url.Parse(c.RedirectURL); err != nil {
		return fmt.Errorf("oidc redirect_url: %w", err)
	}
	if !strings.Contains(c.Scopes, "openid") {
		return errors.New("oidc scopes must include openid")
	}
	return nil
}

// Discovery is the subset of the OpenID Connect discovery document
// this client uses (OpenID Connect Discovery 1.0 §3).
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// Claims is the verified identity of the user: the ID-token claims
// merged with the userinfo response (userinfo fills in profile
// claims the demo IdP does not put in the ID token).
type Claims struct {
	Subject           string
	Name              string
	PreferredUsername string
	Email             string
	Department        string
}

// pendingState is one in-flight authorization request: state + nonce
// + PKCE verifier, single-use, 10-minute expiry.
type pendingState struct {
	nonce    string
	verifier string
	expires  time.Time
}

// Provider is an OIDC relying party bound to one issuer.
type Provider struct {
	cfg    Config
	client *http.Client

	mu        sync.Mutex
	discovery *Discovery
	jwks      *jwksCache
	states    map[string]pendingState
}

type jwksCache struct {
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// NewProvider fetches the discovery document and validates it: the
// issuer in the document must equal the configured issuer (Discovery
// §2.1), and the endpoints must be present. Empty Scopes /
// DepartmentClaim are filled with their defaults.
func NewProvider(cfg Config) (*Provider, error) {
	if cfg.Scopes == "" {
		cfg.Scopes = "openid profile email"
	}
	if cfg.DepartmentClaim == "" {
		cfg.DepartmentClaim = "department"
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	p := &Provider{cfg: cfg, client: client, states: map[string]pendingState{}}
	doc, err := p.fetchDiscovery(context.Background())
	if err != nil {
		return nil, err
	}
	p.discovery = doc
	return p, nil
}

// Config returns the provider configuration (without the secret).
func (p *Provider) Config() Config {
	c := p.cfg
	c.ClientSecret = ""
	return c
}

// Discovery returns the fetched discovery document.
func (p *Provider) Discovery() *Discovery { return p.discovery }

func (p *Provider) fetchDiscovery(ctx context.Context) (*Discovery, error) {
	wellKnown := p.cfg.Issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery %s: %w", wellKnown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery %s: HTTP %d", wellKnown, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var doc Discovery
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if doc.Issuer != p.cfg.Issuer {
		return nil, fmt.Errorf("oidc discovery issuer mismatch: got %q want %q", doc.Issuer, p.cfg.Issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return nil, errors.New("oidc discovery document is missing endpoints")
	}
	return &doc, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthURL starts an authorization-code request (Core §3.1.2.1) and
// returns the URL to redirect the browser to. The returned state must
// be echoed back by the IdP at the callback; it is single-use.
func (p *Provider) AuthURL() (string, error) {
	state, err := randomString(16)
	if err != nil {
		return "", err
	}
	nonce, err := randomString(16)
	if err != nil {
		return "", err
	}
	verifier, err := randomString(32)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.states[state] = pendingState{nonce: nonce, verifier: verifier, expires: time.Now().Add(10 * time.Minute)}
	p.mu.Unlock()

	q := url.Values{}
	q.Set("client_id", p.cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("scope", p.cfg.Scopes)
	q.Set("redirect_uri", p.cfg.RedirectURL)
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	return p.discovery.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// Exchange completes the flow: it validates the returned state
// (single-use), exchanges the code at the token endpoint (Core
// §3.1.3), verifies the ID token (Core §3.1.3.7) and merges the
// userinfo claims (Core §5.3).
func (p *Provider) Exchange(ctx context.Context, code, state string) (*Claims, error) {
	p.mu.Lock()
	pending, ok := p.states[state]
	if ok {
		delete(p.states, state)
	}
	p.mu.Unlock()
	if !ok {
		return nil, errors.New("oidc: unknown or already-used state")
	}
	if time.Now().After(pending.expires) {
		return nil, errors.New("oidc: state expired")
	}
	if code == "" {
		return nil, errors.New("oidc: missing code")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", p.cfg.RedirectURL)
	form.Set("code_verifier", pending.verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.discovery.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(p.cfg.ClientID, p.cfg.ClientSecret)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc token exchange: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("oidc token response: %w", err)
	}
	if tok.IDToken == "" {
		return nil, errors.New("oidc token response has no id_token")
	}
	claims, err := p.VerifyIDToken(ctx, tok.IDToken, pending.nonce)
	if err != nil {
		return nil, err
	}
	// Profile claims: the IdP may carry them only in userinfo
	// (the Duende demo does), so merge what is missing.
	if tok.AccessToken != "" && p.discovery.UserinfoEndpoint != "" {
		if extra, err := p.fetchUserinfo(ctx, tok.AccessToken); err == nil {
			mergeClaims(claims, extra)
		}
		// A userinfo failure does not fail the login: the verified
		// ID token already establishes the identity.
	}
	return claims, nil
}

func mergeClaims(dst, src *Claims) {
	if dst.Name == "" {
		dst.Name = src.Name
	}
	if dst.PreferredUsername == "" {
		dst.PreferredUsername = src.PreferredUsername
	}
	if dst.Email == "" {
		dst.Email = src.Email
	}
	if dst.Department == "" {
		dst.Department = src.Department
	}
}

func (p *Provider) fetchUserinfo(ctx context.Context, accessToken string) (*Claims, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.discovery.UserinfoEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return p.parseClaims(body)
}

// parseClaims extracts the claims we use from an ID-token payload or
// a userinfo response. Only string claims are read; anything else is
// ignored (a department claim that is not a string is not trusted).
func (p *Provider) parseClaims(body []byte) (*Claims, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	c := &Claims{}
	c.Subject, _ = raw["sub"].(string)
	c.Name, _ = raw["name"].(string)
	c.PreferredUsername, _ = raw["preferred_username"].(string)
	c.Email, _ = raw["email"].(string)
	if p.cfg.DepartmentClaim != "" {
		c.Department, _ = raw[p.cfg.DepartmentClaim].(string)
	}
	return c, nil
}

// VerifyIDToken verifies an ID token (Core §3.1.3.7): RS256 signature
// against the IdP JWKS, iss == configured issuer, aud == our client
// ID, exp in the future and nonce matching the authorization request.
// A 5-minute clock skew is tolerated, as the spec suggests.
func (p *Provider) VerifyIDToken(ctx context.Context, raw, nonce string) (*Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("oidc: malformed id_token")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("oidc: bad id_token header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("oidc: bad id_token header: %w", err)
	}
	// Only RS256 is supported: the token must be signed with the
	// IdP's RSA key. Anything else (none, HS256) is rejected outright.
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("oidc: unsupported id_token alg %q", header.Alg)
	}
	if header.Kid == "" {
		return nil, errors.New("oidc: id_token has no kid")
	}
	key, err := p.publicKey(ctx, header.Kid)
	if err != nil {
		return nil, err
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("oidc: bad id_token signature: %w", err)
	}
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return nil, fmt.Errorf("oidc: id_token signature verification failed: %w", err)
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("oidc: bad id_token payload: %w", err)
	}
	claims, err := p.parseClaims(payloadBytes)
	if err != nil {
		return nil, fmt.Errorf("oidc: bad id_token payload: %w", err)
	}
	var rawClaims map[string]any
	if err := json.Unmarshal(payloadBytes, &rawClaims); err != nil {
		return nil, err
	}
	if iss, _ := rawClaims["iss"].(string); iss != p.cfg.Issuer {
		return nil, fmt.Errorf("oidc: id_token issuer mismatch: got %q", iss)
	}
	// aud may be a string or an array of strings (Core §3.1.3.7 #3).
	audOK := false
	switch aud := rawClaims["aud"].(type) {
	case string:
		audOK = aud == p.cfg.ClientID
	case []any:
		for _, a := range aud {
			if s, _ := a.(string); s == p.cfg.ClientID {
				audOK = true
			}
		}
	}
	if !audOK {
		return nil, errors.New("oidc: id_token audience mismatch")
	}
	now := time.Now()
	// exp is required; iat, if present, must not be in the future
	// beyond the skew window.
	exp, _ := rawClaims["exp"].(float64)
	if exp == 0 {
		return nil, errors.New("oidc: id_token has no exp")
	}
	if now.After(time.Unix(int64(exp), 0).Add(5 * time.Minute)) {
		return nil, errors.New("oidc: id_token expired")
	}
	if iat, _ := rawClaims["iat"].(float64); iat != 0 && time.Unix(int64(iat), 0).After(now.Add(5*time.Minute)) {
		return nil, errors.New("oidc: id_token issued in the future")
	}
	if nonce != "" {
		if got, _ := rawClaims["nonce"].(string); got != nonce {
			return nil, errors.New("oidc: id_token nonce mismatch")
		}
	}
	if claims.Subject == "" {
		return nil, errors.New("oidc: id_token has no sub")
	}
	return claims, nil
}

// publicKey returns the RSA key for kid from the JWKS, refreshing the
// cached JWKS on a miss or after 10 minutes.
func (p *Provider) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jwks == nil || time.Since(p.jwks.fetched) > 10*time.Minute {
		keys, err := p.fetchJWKS(ctx)
		if err != nil {
			return nil, err
		}
		p.jwks = &jwksCache{keys: keys, fetched: time.Now()}
	}
	if key, ok := p.jwks.keys[kid]; ok {
		return key, nil
	}
	// Unknown kid: refresh once in case the IdP rotated keys.
	keys, err := p.fetchJWKS(ctx)
	if err != nil {
		return nil, err
	}
	p.jwks = &jwksCache{keys: keys, fetched: time.Now()}
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("oidc: no JWKS key for kid %q", kid)
}

func (p *Provider) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.discovery.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc JWKS: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("oidc JWKS: %w", err)
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.N == "" || k.E == "" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 | int(b)
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}
	}
	if len(out) == 0 {
		return nil, errors.New("oidc JWKS contains no RSA keys")
	}
	return out, nil
}
