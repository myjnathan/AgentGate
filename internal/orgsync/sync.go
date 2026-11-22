package orgsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/store"
)

// Syncer applies a fetched directory to platform accounts. One
// Syncer serves all providers; it keeps the last fetched directory
// per provider in memory so admin binds can be validated against
// what the provider actually returned.
type Syncer struct {
	st        *store.Store
	onDisable func(userID string) // revoke sessions of a disabled user; may be nil

	mu        sync.Mutex
	snapshots map[string]map[string]DirectoryUser // provider ID -> external ID -> user
}

// New builds a Syncer over the store. onDisable is called (if
// non-nil) for every account the sync disables, so open sessions
// stop working at once.
func New(st *store.Store, onDisable func(userID string)) *Syncer {
	return &Syncer{st: st, onDisable: onDisable, snapshots: map[string]map[string]DirectoryUser{}}
}

// Snapshot returns the last fetched directory for a provider, if
// it has ever synced successfully.
func (s *Syncer) Snapshot(providerID string) (map[string]DirectoryUser, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snapshots[providerID]
	if !ok {
		return nil, false
	}
	out := make(map[string]DirectoryUser, len(snap))
	for k, v := range snap {
		out[k] = v
	}
	return out, true
}

// corpKey is the second segment of the external subject: the DingTalk
// corpId / WeCom corpid, or the Feishu app_id (Feishu has no corp).
func corpKey(cfg Config) string {
	if cfg.Provider == ProviderFeishu {
		return cfg.AppKey
	}
	return cfg.CorpID
}

// deptNames renders a remote user's departments as the platform
// department string ("A / B"), like dsh-ee's dingTalkDeptNames.
func deptNames(dir *Directory, ids []string) string {
	var names []string
	for _, id := range ids {
		if name := strings.TrimSpace(dir.Departments[id]); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, " / ")
}

// deriveUsername mirrors auth.oidcUsername: email local part first,
// sanitised to [a-z0-9._-], with a subject-hash fallback.
func deriveUsername(email, externalID, subject string) string {
	cand := ""
	if email != "" {
		cand = strings.SplitN(email, "@", 2)[0]
	}
	if cand == "" {
		cand = externalID
	}
	var b strings.Builder
	for _, r := range strings.ToLower(cand) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		h := sha256.Sum256([]byte(subject))
		return "org-" + hex.EncodeToString(h[:])[:8]
	}
	return b.String()
}

// Sync runs one full sync for a provider. The directory fetch must
// succeed completely or nothing changes; an empty directory aborts
// the run (dsh-ee rule: never let a bad fetch wipe accounts). The
// returned error is only for store/system failures; a failed fetch
// or an empty directory is reported in result.OK/result.Error.
// The result is persisted as the provider's LastSync.
func (s *Syncer) Sync(ctx context.Context, providerID string, cfg Config, client DirectoryClient) (*store.OrgSyncResult, error) {
	res := &store.OrgSyncResult{
		At: time.Now().UTC(), Disabled: []string{}, Kept: []string{},
		Unlinked: []string{}, Conflicts: []string{}, Skipped: []string{},
	}
	finish := func() (*store.OrgSyncResult, error) {
		s.st.WithLock(func(st *store.State) {
			if p, ok := st.OrgProviders[providerID]; ok {
				p.LastSync = res
				p.UpdatedAt = time.Now().UTC()
			}
		})
		if err := s.st.Save(); err != nil {
			return nil, err
		}
		return res, nil
	}

	dir, err := client.Directory(ctx)
	if err != nil {
		res.Error = err.Error()
		return finish()
	}
	if len(dir.Users) == 0 {
		res.Error = "directory has no users; refusing to update or disable accounts"
		return finish()
	}
	res.Remote = len(dir.Users)

	// Remember the fetched directory for bind validation.
	snap := make(map[string]DirectoryUser, len(dir.Users))
	for _, u := range dir.Users {
		snap[u.ExternalID] = u
	}
	s.mu.Lock()
	s.snapshots[providerID] = snap
	s.mu.Unlock()

	key := corpKey(cfg)
	prefix := string(cfg.Provider) + ":" + key + ":"
	seen := map[string]bool{}
	for _, remote := range dir.Users {
		subject := Subject(cfg.Provider, key, remote.ExternalID)
		if len(subject) > 512 {
			res.Conflicts = append(res.Conflicts, remote.Name+"（external id too long）")
			continue
		}
		seen[subject] = true
		dept := deptNames(dir, remote.DeptIDs)

		var existing *store.User
		var linked bool
		s.st.WithLock(func(st *store.State) {
			for _, u := range st.Users {
				for _, id := range u.ExternalIdentities {
					if id.Subject == subject {
						cp := *u
						existing = &cp
					}
				}
			}
			if existing != nil {
				return
			}
			// Email auto-link (dsh-ee autoLink): the directory
			// identity joins the platform account that already
			// has this corporate mailbox, instead of creating a
			// duplicate. The mailbox must match exactly.
			if remote.Email != "" {
				for _, u := range st.Users {
					if u.Email != "" && strings.EqualFold(u.Email, remote.Email) {
						cp := *u
						cp.ExternalIdentities = append(append([]store.ExternalIdentity(nil), cp.ExternalIdentities...),
							store.ExternalIdentity{Provider: string(cfg.Provider), ExternalID: remote.ExternalID, Subject: subject})
						if remote.Name != "" {
							cp.DisplayName = remote.Name
						}
						if dept != "" {
							cp.Department = dept
						}
						st.Users[cp.ID] = &cp
						existing = &cp
						linked = true
						break
					}
				}
			}
		})
		if linked {
			res.Linked++
			continue
		}
		if existing != nil && existing.Disabled {
			res.Skipped = append(res.Skipped, existing.Username+"（本地已禁用，不自动恢复）")
			continue
		}
		if existing == nil && !cfg.AutoCreate {
			res.Skipped = append(res.Skipped, deriveUsername(remote.Email, remote.ExternalID, subject)+"（未预先开通）")
			continue
		}

		username := ""
		if existing == nil {
			username = deriveUsername(remote.Email, remote.ExternalID, subject)
		}
		s.st.WithLock(func(st *store.State) {
			if existing != nil {
				u := st.Users[existing.ID]
				if u == nil {
					return
				}
				if remote.Name != "" {
					u.DisplayName = remote.Name
				}
				if dept != "" {
					u.Department = dept
				}
				if remote.Email != "" {
					u.Email = remote.Email
				}
				cp := *u
				existing = &cp
				return
			}
			// Never collide with another account's username.
			if _, taken := usernameTaken(st, username); taken {
				h := sha256.Sum256([]byte(subject))
				username = username + "-" + hex.EncodeToString(h[:])[:8]
			}
			u := &store.User{
				ID: store.NewID("usr"), Username: username,
				DisplayName: remote.Name, Department: dept,
				Role: "employee", Email: remote.Email,
				AuthSource: string(cfg.Provider),
				ExternalIdentities: []store.ExternalIdentity{
					{Provider: string(cfg.Provider), ExternalID: remote.ExternalID, Subject: subject},
				},
			}
			if u.DisplayName == "" {
				u.DisplayName = username
			}
			st.Users[u.ID] = u
			cp := *u
			existing = &cp
		})
		if existing == nil {
			// The account vanished between the lookup and the
			// write (no delete path exists today); record it as a
			// conflict instead of silently dropping the member.
			res.Conflicts = append(res.Conflicts, username)
			continue
		}
		if username != "" {
			res.Created++
		} else {
			res.Reconciled++
		}
	}
	if err := s.st.Save(); err != nil {
		return nil, err
	}

	if cfg.DisableMissing {
		if err := s.disableMissing(ctx, cfg, prefix, seen, client, res); err != nil {
			res.Error = err.Error()
			return finish()
		}
	}
	res.OK = true
	return finish()
}

func usernameTaken(st *store.State, username string) (*store.User, bool) {
	for _, u := range st.Users {
		if u.Username == username {
			return u, true
		}
	}
	return nil, false
}

// disableMissing confirms every platform account carrying this
// provider's identity but absent from the fetched directory.
// Absence alone is not departure (dsh-ee rule): only a provider
// that explicitly says "not found" disables an account, and only
// an account the provider itself created is disabled outright;
// an identity merely linked to another source's account is just
// unlinked, the account stays with its home source.
func (s *Syncer) disableMissing(ctx context.Context, cfg Config, prefix string, seen map[string]bool, client DirectoryClient, res *store.OrgSyncResult) error {
	var targets []*store.User
	s.st.Read(func(st *store.State) {
		for _, u := range st.Users {
			if u.Disabled {
				continue
			}
			for _, id := range u.ExternalIdentities {
				if strings.HasPrefix(id.Subject, prefix) && !seen[id.Subject] {
					cp := *u
					targets = append(targets, &cp)
					break
				}
			}
		}
	})
	for _, u := range targets {
		var extID string
		for _, id := range u.ExternalIdentities {
			if strings.HasPrefix(id.Subject, prefix) && !seen[id.Subject] {
				extID = id.ExternalID
				break
			}
		}
		state, err := client.UserState(ctx, extID)
		if err != nil {
			return fmt.Errorf("confirming %s: %w", u.Username, err)
		}
		if state != UserGone {
			why := "仍在职，但不在这次拉到的目录里"
			if state == UserOutOfScope {
				why = "不在应用的通讯录权限范围内，看不到状态"
			}
			res.Kept = append(res.Kept, u.Username+"（"+why+"）")
			continue
		}
		disabled := false
		s.st.WithLock(func(st *store.State) {
			stored := st.Users[u.ID]
			if stored == nil || stored.Disabled {
				return
			}
			if stored.AuthSource == string(cfg.Provider) {
				stored.Disabled = true
				disabled = true
			} else {
				kept := make([]store.ExternalIdentity, 0, len(stored.ExternalIdentities))
				for _, id := range stored.ExternalIdentities {
					if !strings.HasPrefix(id.Subject, prefix) || seen[id.Subject] {
						kept = append(kept, id)
					}
				}
				stored.ExternalIdentities = kept
			}
		})
		if disabled {
			if s.onDisable != nil {
				s.onDisable(u.ID)
			}
			res.Disabled = append(res.Disabled, u.Username)
		} else {
			res.Unlinked = append(res.Unlinked, u.Username)
		}
	}
	return nil
}
