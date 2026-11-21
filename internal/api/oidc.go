package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/oidc"
	"github.com/myjnathan/agentgate/internal/store"
)

// SetOIDC wires the OIDC provider (backlog F9). A nil provider means
// OIDC is not configured; local login stays the only way in.
func (a *API) SetOIDC(p *oidc.Provider) {
	a.oidc = p
}

// oidcProviders reports which login methods the web console offers.
// The client secret is never included.
func (a *API) oidcProviders(w http.ResponseWriter, r *http.Request) {
	enabled := a.oidc != nil
	issuer := ""
	if enabled {
		issuer = a.oidc.Config().Issuer
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"local_enabled": true,
		"oidc_enabled":  enabled,
		"oidc_issuer":   issuer,
	})
}

// oidcStatus is the admin view of the OIDC configuration (no secret).
func (a *API) oidcStatus(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.oidc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"oidc_enabled": false})
		return
	}
	cfg := a.oidc.Config()
	disc := a.oidc.Discovery()
	writeJSON(w, http.StatusOK, map[string]any{
		"oidc_enabled":           true,
		"issuer":                 cfg.Issuer,
		"client_id":              cfg.ClientID,
		"redirect_url":           cfg.RedirectURL,
		"scopes":                 cfg.Scopes,
		"department_claim":       cfg.DepartmentClaim,
		"authorization_endpoint": disc.AuthorizationEndpoint,
		"token_endpoint":         disc.TokenEndpoint,
		"userinfo_endpoint":      disc.UserinfoEndpoint,
	})
}

// oidcLogin starts the authorization-code flow: redirect the browser
// to the IdP. The state/nonce/PKCE values live server-side, keyed by
// the state the IdP echoes back.
func (a *API) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if a.oidc == nil {
		writeErr(w, http.StatusServiceUnavailable, "oidc not configured")
		return
	}
	url, err := a.oidc.AuthURL()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to start oidc login")
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// oidcCallback completes the flow: exchange the code, verify the ID
// token, JIT-provision the user and issue a web session. On success
// the browser receives a page that stores the session token and
// returns to the console; on failure a generic error page (no
// upstream detail, no code/state is logged).
func (a *API) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if a.oidc == nil {
		writeErr(w, http.StatusServiceUnavailable, "oidc not configured")
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		a.audit.Append(audit.Event{Actor: "oidc", Kind: "auth", Action: "oidc_login_failed", Decision: "block",
			Detail: map[string]string{"error": "idp_returned_error"}})
		oidcErrorPage(w, "企业登录失败，请重试或改用本地账号登录。")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	claims, err := a.oidc.Exchange(ctx, code, state)
	if err != nil {
		a.audit.Append(audit.Event{Actor: "oidc", Kind: "auth", Action: "oidc_login_failed", Decision: "block"})
		oidcErrorPage(w, "企业登录失败，请重试或改用本地账号登录。")
		return
	}
	user, err := a.auth.LoginOIDC(auth.OIDCClaims{
		Subject: claims.Subject, PreferredUsername: claims.PreferredUsername,
		Email: claims.Email, DisplayName: claims.Name, Department: claims.Department,
	})
	if err != nil {
		a.audit.Append(audit.Event{Actor: "oidc", Kind: "auth", Action: "oidc_login_failed", Decision: "block"})
		oidcErrorPage(w, "企业登录失败：账号不可用，请联系管理员。")
		return
	}
	tok := a.auth.CreateSession(user)
	a.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, Kind: "auth", Action: "oidc_login", Decision: "allow",
		Detail: map[string]string{"auth_source": "oidc"}})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>登录成功</title></head><body><script>
localStorage.setItem("ag_session", %s);
location.href = "/";
</script><p>登录成功，正在返回…</p></body></html>`, jsonString(tok))
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func oidcErrorPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>登录失败</title></head><body><p>%s</p><p><a href="/">返回登录页</a></p></body></html>`,
		strings.ReplaceAll(msg, "<", "&lt;"))
}
