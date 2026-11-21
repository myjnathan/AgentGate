package api_test

import (
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

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/oidc"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// oidcTestServer builds a server with the API registered and, unless
// disabled, an OIDC provider pointed at a fake IdP whose authorize
// endpoint immediately issues a code bound to the request nonce.
func oidcTestServer(t *testing.T, withOIDC bool) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New("test-master-key-123456")
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(t.TempDir() + "/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	au := auth.New(st)
	rest := api.New(st, au, box, al)

	mux := http.NewServeMux()
	rest.Register(mux)

	if withOIDC {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		codes := map[string]string{} // code -> nonce
		idp := http.NewServeMux()
		var issuer string
		idp.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
				"token_endpoint": issuer + "/token", "userinfo_endpoint": issuer + "/userinfo",
				"jwks_uri": issuer + "/jwks",
			})
		})
		idp.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
			n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
			e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{
				{"kid": "k1", "kty": "RSA", "n": n, "e": e},
			}})
		})
		idp.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
			// The fake IdP "authenticates" immediately and returns a code.
			q := r.URL.Query()
			code := "code-" + q.Get("state")
			codes[code] = q.Get("nonce")
			back, _ := url.Parse(q.Get("redirect_uri"))
			bq := back.Query()
			bq.Set("code", code)
			bq.Set("state", q.Get("state"))
			back.RawQuery = bq.Encode()
			http.Redirect(w, r, back.String(), http.StatusFound)
		})
		idp.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			nonce := codes[r.Form.Get("code")]
			now := time.Now()
			header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"})
			payload, _ := json.Marshal(map[string]any{
				"iss": issuer, "aud": "test-client", "sub": "idp-user-1",
				"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": nonce,
			})
			enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
			input := enc(header) + "." + enc(payload)
			sum := sha256.Sum256([]byte(input))
			sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id_token": input + "." + enc(sig), "access_token": "at-1", "token_type": "Bearer",
			})
		})
		idp.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sub": "idp-user-1", "name": "IdP User", "preferred_username": "idpuser",
				"email": "idpuser@example.test", "department": "IT",
			})
		})
		idpSrv := httptest.NewServer(idp)
		t.Cleanup(idpSrv.Close)
		issuer = idpSrv.URL
		// The callback URL must match the test server's own URL, which
		// is only known after it starts — so start the API server
		// first, then create the provider with its real callback URL.
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		cfg := oidc.Config{Issuer: issuer, ClientID: "test-client", ClientSecret: "test-secret",
			RedirectURL: srv.URL + "/auth/oidc/callback", Scopes: "openid profile email"}
		p, err := oidc.NewProvider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		rest.SetOIDC(p)
		return srv, st
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

func TestOIDCProvidersDisabled(t *testing.T) {
	srv, _ := oidcTestServer(t, false)
	resp, err := http.Get(srv.URL + "/api/auth/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["oidc_enabled"] != false || body["local_enabled"] != true {
		t.Fatalf("providers = %v", body)
	}
	// Login entry point reports OIDC as unavailable.
	resp2, err := http.Get(srv.URL + "/auth/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("login without OIDC = %d, want 503", resp2.StatusCode)
	}
	// Callback with a bad state fails closed with a generic page.
	resp3, err := http.Get(srv.URL + "/auth/oidc/callback?code=x&state=y")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("callback without OIDC = %d, want 503", resp3.StatusCode)
	}
}

func TestOIDCFullLoginLoop(t *testing.T) {
	srv, st := oidcTestServer(t, true)

	// Follow the login redirect: fake IdP -> callback -> session page.
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	resp, err := client.Get(srv.URL + "/auth/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login loop final status = %d", resp.StatusCode)
	}
	bodyBytes := make([]byte, 1<<20)
	n, _ := resp.Body.Read(bodyBytes)
	body := string(bodyBytes[:n])
	if !strings.Contains(body, `localStorage.setItem("ag_session"`) {
		t.Fatalf("callback page must deliver a session, got: %.200s", body)
	}
	// The OIDC user was JIT-provisioned as an employee.
	var found *store.User
	st.Read(func(s *store.State) {
		for _, u := range s.Users {
			if u.OIDCSubject == "idp-user-1" {
				cp := *u
				found = &cp
			}
		}
	})
	if found == nil {
		t.Fatal("OIDC user was not provisioned")
	}
	if found.Username != "idpuser" || found.Role != "employee" || found.Department != "IT" {
		t.Fatalf("provisioned user mismatch: %+v", found)
	}
	// The delivered session resolves via /api/me.
	start := strings.Index(body, `ags_`)
	if start < 0 {
		t.Fatalf("no session token in callback page: %.200s", body)
	}
	end := strings.Index(body[start:], `"`)
	tok := body[start : start+end]
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	meResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer meResp.Body.Close()
	var me map[string]any
	if err := json.NewDecoder(meResp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me["username"] != "idpuser" || me["role"] != "employee" {
		t.Fatalf("/api/me = %v", me)
	}
}

func TestOIDCCallbackBadState(t *testing.T) {
	srv, _ := oidcTestServer(t, true)
	resp, err := http.Get(srv.URL + "/auth/oidc/callback?code=x&state=unknown-state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state callback = %d, want 400", resp.StatusCode)
	}
}

func TestOIDCStatusRequiresAdmin(t *testing.T) {
	srv, _ := oidcTestServer(t, true)
	resp, err := http.Get(srv.URL + "/api/auth/oidc/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}
}
