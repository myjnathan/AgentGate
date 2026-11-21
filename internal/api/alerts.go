// Package api — alert webhook management (F5).
//
// GET /api/alerts/webhooks lists configured webhooks (admin/security
// only); POST creates one; DELETE removes one; POST
// /api/alerts/webhooks/{id}/test sends a test alert through the
// dispatcher synchronously. Signing secrets are encrypted at rest
// with the master key and never returned; webhook URLs are shown
// masked (query values carry bot keys).
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// webhookView is the admin-facing webhook shape: metadata, a masked
// URL and the last delivery outcome. Never the signing secret.
type webhookView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	URL           string     `json:"url"`
	Kind          string     `json:"kind"`
	HasSecret     bool       `json:"has_secret"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastOK        *bool      `json:"last_ok,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	Attempts      int        `json:"attempts,omitempty"`
}

func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	for k := range q {
		q[k] = []string{"***"}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (a *API) webhookViews() []webhookView {
	var status map[string]alert.DeliveryStatus
	if a.dispatcher != nil {
		status = a.dispatcher.Status()
	}
	var out []webhookView
	a.st.Read(func(st *store.State) {
		for _, wh := range st.Webhooks {
			v := webhookView{ID: wh.ID, Name: wh.Name, URL: maskURL(wh.URL),
				Kind: wh.Kind, HasSecret: wh.SecretEnc != "", Enabled: wh.Enabled,
				CreatedAt: wh.CreatedAt}
			if s, ok := status[wh.ID]; ok {
				t := s.LastAttemptAt
				v.LastAttemptAt = &t
				ok := s.LastOK
				v.LastOK = &ok
				v.LastError = s.LastError
				v.Attempts = s.Attempts
			}
			out = append(out, v)
		}
	})
	return out
}

func (a *API) listWebhooks(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": a.webhookViews()})
}

func (a *API) createWebhook(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		Kind   string `json:"kind"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.URL == "" {
		writeErr(w, http.StatusBadRequest, "name and url required")
		return
	}
	if !alert.ValidKind(in.Kind) {
		writeErr(w, http.StatusBadRequest, "kind must be generic, wecom, dingtalk or feishu")
		return
	}
	if !strings.HasPrefix(in.URL, "https://") && !strings.HasPrefix(in.URL, "http://127.0.0.1") && !strings.HasPrefix(in.URL, "http://localhost") {
		writeErr(w, http.StatusBadRequest, "url must be https (or loopback http for testing)")
		return
	}
	enc := ""
	if in.Secret != "" {
		var err error
		enc, err = a.box.Encrypt(in.Secret)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "encrypt failed")
			return
		}
	}
	wh := &store.Webhook{ID: store.NewID("wh"), Name: in.Name, URL: in.URL,
		Kind: in.Kind, SecretEnc: enc, Enabled: true, CreatedAt: time.Now()}
	a.st.WithLock(func(st *store.State) { st.Webhooks[wh.ID] = wh })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "webhook_created",
		Detail: map[string]string{"webhook_id": wh.ID, "kind": wh.Kind}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": wh.ID, "has_secret": enc != ""})
}

func (a *API) deleteWebhook(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	found := false
	a.st.WithLock(func(st *store.State) {
		if _, ok := st.Webhooks[id]; ok {
			delete(st.Webhooks, id)
			found = true
		}
	})
	if !found {
		writeErr(w, http.StatusNotFound, "webhook not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "webhook_deleted",
		Detail: map[string]string{"webhook_id": id}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (a *API) testWebhook(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var wh *store.Webhook
	a.st.Read(func(st *store.State) {
		if x, ok := st.Webhooks[id]; ok {
			cp := *x
			wh = &cp
		}
	})
	if wh == nil {
		writeErr(w, http.StatusNotFound, "webhook not found")
		return
	}
	if a.dispatcher == nil {
		writeErr(w, http.StatusServiceUnavailable, "alert dispatcher not configured")
		return
	}
	attempts, err, status := a.dispatcher.Send(*wh, alert.TestAlert(u.Username))
	detail := map[string]string{"webhook_id": id,
		"result": map[bool]string{true: "delivered", false: "failed"}[err == nil]}
	if err != nil {
		detail["error"] = alert.ErrClass(err, status)
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "alert_test_sent",
		Detail: detail})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"delivered": false, "attempts": attempts, "error": alert.ErrClass(err, status)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivered": true, "attempts": attempts})
}
