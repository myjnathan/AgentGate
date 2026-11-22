// Package api — organisation sync (F21).
//
// Third-party directory providers (DingTalk / Feishu / WeCom) sync
// departments and employees into platform accounts and bind the
// third-party identity to the account, in parallel with F9 OIDC.
// The app secret is encrypted with the master key at rest and never
// echoed back; sync only creates/updates accounts and department
// mappings, never grants a tier (that stays with platform policy).
package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/orgsync"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// SetOrgSync wires the org sync engine (backlog F21).
func (a *API) SetOrgSync(s *orgsync.Syncer) {
	a.orgsync = s
}

// providerView is the API shape of a provider: the encrypted secret
// is never included, only its masked form.
func providerView(p *store.OrgProvider) map[string]any {
	return map[string]any{
		"id": p.ID, "provider": p.Provider, "name": p.Name,
		"corp_id": p.CorpID, "app_key": p.AppKey, "secret_masked": p.SecretMasked,
		"base_url": p.BaseURL, "enabled": p.Enabled,
		"auto_create": p.AutoCreate, "disable_missing": p.DisableMissing,
		"created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
		"last_sync": p.LastSync,
	}
}

// orgsyncConfig decrypts a provider record into a runnable client
// config. The secret only lives in this call's memory.
func (a *API) orgsyncConfig(p *store.OrgProvider) (orgsync.Config, error) {
	prov, _ := orgsync.ParseProvider(p.Provider)
	secret, err := a.box.Decrypt(p.SecretEnc)
	if err != nil {
		return orgsync.Config{}, err
	}
	return orgsync.Config{
		Provider: prov, CorpID: p.CorpID, AppKey: p.AppKey,
		AppSecret: secret, BaseURL: p.BaseURL,
		AutoCreate: p.AutoCreate, DisableMissing: p.DisableMissing,
	}, nil
}

// listOrgProviders handles GET /api/orgsync/providers.
func (a *API) listOrgProviders(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []map[string]any
	a.st.Read(func(st *store.State) {
		for _, p := range st.OrgProviders {
			out = append(out, providerView(p))
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

// createOrgProvider handles POST /api/orgsync/providers.
func (a *API) createOrgProvider(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Provider       string `json:"provider"`
		Name           string `json:"name"`
		CorpID         string `json:"corp_id"`
		AppKey         string `json:"app_key"`
		AppSecret      string `json:"app_secret"`
		BaseURL        string `json:"base_url"`
		Enabled        *bool  `json:"enabled"`
		AutoCreate     bool   `json:"auto_create"`
		DisableMissing bool   `json:"disable_missing"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	prov, ok := orgsync.ParseProvider(in.Provider)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown provider (dingtalk, feishu or wecom)")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.CorpID = strings.TrimSpace(in.CorpID)
	in.AppKey = strings.TrimSpace(in.AppKey)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if in.AppSecret == "" {
		writeErr(w, http.StatusBadRequest, "app_secret is required")
		return
	}
	cfg := orgsync.Config{Provider: prov, CorpID: in.CorpID, AppKey: in.AppKey,
		AppSecret: in.AppSecret, BaseURL: in.BaseURL}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enc, err := a.box.Encrypt(in.AppSecret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot store secret")
		return
	}
	now := time.Now().UTC()
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	p := &store.OrgProvider{
		ID: store.NewID("org"), Provider: string(prov), Name: in.Name,
		CorpID: in.CorpID, AppKey: in.AppKey, SecretEnc: enc,
		SecretMasked: secretbox.Mask(in.AppSecret), BaseURL: in.BaseURL,
		Enabled: enabled, AutoCreate: in.AutoCreate,
		DisableMissing: in.DisableMissing, CreatedAt: now, UpdatedAt: now,
	}
	a.st.WithLock(func(st *store.State) {
		st.OrgProviders[p.ID] = p
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID,
		Kind: "admin", Action: "org_provider_create", Decision: "allow",
		Detail: map[string]string{"provider": p.Provider, "provider_id": p.ID, "name": p.Name}})
	writeJSON(w, http.StatusCreated, map[string]any{"provider": providerView(p)})
}

// getOrgProvider handles GET /api/orgsync/providers/{id}.
func (a *API) getOrgProvider(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var p *store.OrgProvider
	a.st.Read(func(st *store.State) {
		if v, ok := st.OrgProviders[id]; ok {
			cp := *v
			p = &cp
		}
	})
	if p == nil {
		writeErr(w, http.StatusNotFound, "no such provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": providerView(p)})
}

// deleteOrgProvider handles DELETE /api/orgsync/providers/{id}.
// Deleting a provider never deletes accounts; identities bound
// through it stay on the accounts (they can be unbound explicitly).
func (a *API) deleteOrgProvider(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var ok bool
	var name string
	a.st.WithLock(func(st *store.State) {
		if p, exists := st.OrgProviders[id]; exists {
			name = p.Name
			delete(st.OrgProviders, id)
			ok = true
		}
	})
	if !ok {
		writeErr(w, http.StatusNotFound, "no such provider")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID,
		Kind: "admin", Action: "org_provider_delete", Decision: "allow",
		Detail: map[string]string{"provider_id": id, "name": name}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// syncOrgProvider handles POST /api/orgsync/providers/{id}/sync: one
// full directory sync. The result is returned even when the run
// failed (ok=false + error); only store failures are a 500.
func (a *API) syncOrgProvider(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var p *store.OrgProvider
	a.st.Read(func(st *store.State) {
		if v, ok := st.OrgProviders[id]; ok {
			cp := *v
			p = &cp
		}
	})
	if p == nil {
		writeErr(w, http.StatusNotFound, "no such provider")
		return
	}
	if !p.Enabled {
		writeErr(w, http.StatusBadRequest, "provider is disabled")
		return
	}
	if a.orgsync == nil {
		writeErr(w, http.StatusInternalServerError, "org sync is not configured")
		return
	}
	cfg, err := a.orgsyncConfig(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot decrypt provider secret")
		return
	}
	client, err := orgsync.NewClient(cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := a.orgsync.Sync(r.Context(), p.ID, cfg, client)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "sync failed")
		return
	}
	decision := "allow"
	if !res.OK {
		decision = "deny"
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID,
		Kind: "orgsync", Action: "org_sync", Decision: decision,
		Detail: map[string]string{
			"provider": p.Provider, "provider_id": p.ID,
			"remote": strconv.Itoa(res.Remote), "created": strconv.Itoa(res.Created),
			"reconciled": strconv.Itoa(res.Reconciled), "linked": strconv.Itoa(res.Linked),
			"disabled": strconv.Itoa(len(res.Disabled)), "kept": strconv.Itoa(len(res.Kept)),
			"unlinked":  strconv.Itoa(len(res.Unlinked)),
			"conflicts": strconv.Itoa(len(res.Conflicts)), "skipped": strconv.Itoa(len(res.Skipped)),
		}})
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

// testOrgProvider handles POST /api/orgsync/providers/{id}/test:
// fetch the directory and report its size, without provisioning
// anyone and without writing audit (same as the upstream/REST-tool
// test endpoints).
func (a *API) testOrgProvider(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var p *store.OrgProvider
	a.st.Read(func(st *store.State) {
		if v, ok := st.OrgProviders[id]; ok {
			cp := *v
			p = &cp
		}
	})
	if p == nil {
		writeErr(w, http.StatusNotFound, "no such provider")
		return
	}
	cfg, err := a.orgsyncConfig(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot decrypt provider secret")
		return
	}
	client, err := orgsync.NewClient(cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dir, err := client.Directory(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "departments": len(dir.Departments), "users": len(dir.Users),
	})
}

// bindOrgIdentity handles POST /api/orgsync/bindings: an admin binds
// a platform user to a third-party identity. The external id must
// exist in a synced directory snapshot of that provider, so a bind
// can never point at an identity the provider never returned.
func (a *API) bindOrgIdentity(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		UserID     string `json:"user_id"`
		Provider   string `json:"provider"`
		ExternalID string `json:"external_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	prov, ok := orgsync.ParseProvider(in.Provider)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown provider (dingtalk, feishu or wecom)")
		return
	}
	in.ExternalID = strings.TrimSpace(in.ExternalID)
	if in.UserID == "" || in.ExternalID == "" {
		writeErr(w, http.StatusBadRequest, "user_id and external_id are required")
		return
	}
	if a.orgsync == nil {
		writeErr(w, http.StatusInternalServerError, "org sync is not configured")
		return
	}
	var target *store.User
	a.st.Read(func(st *store.State) {
		if v, ok := st.Users[in.UserID]; ok {
			cp := *v
			target = &cp
		} else {
			// The admin console binds by username; accept that too.
			for _, v := range st.Users {
				if v.Username == in.UserID {
					cp := *v
					target = &cp
					break
				}
			}
		}
	})
	if target == nil {
		writeErr(w, http.StatusNotFound, "no such user")
		return
	}
	// Candidate subjects: every synced provider of this type whose
	// snapshot holds the external id. Several providers (two corps)
	// can hold the same external id, so collect all candidates in
	// provider-ID order and prefer one that is not already bound to
	// another account; the choice is deterministic, never map order.
	var subjects []string
	a.st.Read(func(st *store.State) {
		ids := make([]string, 0, len(st.OrgProviders))
		for id := range st.OrgProviders {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			p := st.OrgProviders[id]
			if p.Provider != string(prov) {
				continue
			}
			snap, ok := a.orgsync.Snapshot(p.ID)
			if !ok {
				continue
			}
			if _, found := snap[in.ExternalID]; !found {
				continue
			}
			key := p.CorpID
			if prov == orgsync.ProviderFeishu {
				key = p.AppKey
			}
			subjects = append(subjects, orgsync.Subject(prov, key, in.ExternalID))
		}
	})
	if len(subjects) == 0 {
		writeErr(w, http.StatusConflict, "external identity not found in any synced directory (sync the provider first)")
		return
	}
	subject := ""
	conflict := ""
	a.st.Read(func(st *store.State) {
		for _, cand := range subjects {
			boundElsewhere := ""
			boundHere := false
			for _, other := range st.Users {
				for _, id := range other.ExternalIdentities {
					if id.Subject != cand {
						continue
					}
					if other.ID == target.ID {
						boundHere = true
					} else {
						boundElsewhere = other.Username
					}
				}
			}
			if boundHere {
				subject = cand
				return
			}
			if boundElsewhere != "" {
				if conflict == "" {
					conflict = boundElsewhere
				}
				continue
			}
			subject = cand
			return
		}
	})
	if subject == "" {
		writeErr(w, http.StatusConflict, "identity already bound to "+conflict)
		return
	}
	alreadyBound := false
	a.st.WithLock(func(st *store.State) {
		tgt := st.Users[target.ID]
		if tgt == nil {
			return
		}
		for _, id := range tgt.ExternalIdentities {
			if id.Subject == subject {
				alreadyBound = true
				return
			}
		}
		tgt.ExternalIdentities = append(tgt.ExternalIdentities, store.ExternalIdentity{
			Provider: string(prov), ExternalID: in.ExternalID, Subject: subject,
		})
		alreadyBound = true
	})
	if !alreadyBound {
		writeErr(w, http.StatusNotFound, "no such user")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID,
		Kind: "orgsync", Action: "org_identity_bound", Decision: "allow",
		Detail: map[string]string{"user_id": target.ID, "username": target.Username,
			"provider": string(prov), "external_id": in.ExternalID}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "bound", "subject": subject})
}

// unbindOrgIdentity handles DELETE /api/orgsync/bindings/{user_id}/{provider}.
func (a *API) unbindOrgIdentity(w http.ResponseWriter, r *http.Request, u *store.User) {
	userID := r.PathValue("user_id")
	prov, ok := orgsync.ParseProvider(r.PathValue("provider"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown provider (dingtalk, feishu or wecom)")
		return
	}
	// Accept a username as well as a user ID, like the bind endpoint.
	a.st.Read(func(st *store.State) {
		if _, exists := st.Users[userID]; !exists {
			for _, v := range st.Users {
				if v.Username == userID {
					userID = v.ID
					break
				}
			}
		}
	})
	removed := false
	a.st.WithLock(func(st *store.State) {
		tgt, exists := st.Users[userID]
		if !exists {
			return
		}
		kept := make([]store.ExternalIdentity, 0, len(tgt.ExternalIdentities))
		for _, id := range tgt.ExternalIdentities {
			if id.Provider == string(prov) {
				removed = true
				continue
			}
			kept = append(kept, id)
		}
		tgt.ExternalIdentities = kept
	})
	if !removed {
		writeErr(w, http.StatusNotFound, "no such identity")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID,
		Kind: "orgsync", Action: "org_identity_unbound", Decision: "allow",
		Detail: map[string]string{"user_id": userID, "provider": string(prov)}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "unbound"})
}

// listMyOrgBindings handles GET /api/orgsync/bindings: the current
// session user's own third-party identities.
func (a *API) listMyOrgBindings(w http.ResponseWriter, r *http.Request, u *store.User) {
	out := u.ExternalIdentities
	if out == nil {
		out = []store.ExternalIdentity{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": out})
}
