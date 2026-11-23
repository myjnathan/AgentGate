package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/roles"
	"github.com/myjnathan/agentgate/internal/store"
)

// User management (backlog F26): the console surface for tiered
// administrators. Listing is admin/security; changing a user's role,
// department or disabled flag is admin only — even the security
// administrator cannot grant roles, so nobody can promote
// themselves. Every change is audited (actor, target, role
// before/after); password hashes are never returned.

// userView is the safe wire shape of a user: everything except the
// password hash.
type userView struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Department  string `json:"department"`
	Role        string `json:"role"`
	RoleSource  string `json:"role_source"`
	AuthSource  string `json:"auth_source"`
	Email       string `json:"email,omitempty"`
	Disabled    bool   `json:"disabled"`
}

func toUserView(u *store.User) userView {
	src := u.RoleSource
	if src == "" {
		// Legacy rows predate the field: local accounts are
		// locally managed, OIDC-provisioned accounts are
		// IdP-managed (they were JIT-created by the OIDC flow).
		if u.AuthSource == "oidc" {
			src = "oidc"
		} else {
			src = "local"
		}
	}
	return userView{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName,
		Department: u.Department, Role: u.Role, RoleSource: src,
		AuthSource: u.AuthSource, Email: u.Email, Disabled: u.Disabled}
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request, u *store.User) {
	users := []userView{}
	a.st.Read(func(st *store.State) {
		for _, usr := range st.Users {
			users = append(users, toUserView(usr))
		}
	})
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// listRoles returns the role matrix description cards for the
// console (same content as docs/roles.md).
func (a *API) listRoles(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"roles": roles.Descriptions()})
}

// updateUser changes role / department / disabled for one user.
// Guards (all 400, not 403: the caller is already an admin, the
// request itself is unsafe):
//   - nobody changes their own role or disables themselves;
//   - the last enabled admin cannot be demoted or disabled;
//   - the role must be a known role.
//
// A role change marks the account locally managed (RoleSource
// "local") so a later OIDC login never overwrites an admin decision.
// Disabling a user revokes their open web sessions at once.
func (a *API) updateUser(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var in struct {
		Role       *string `json:"role"`
		Department *string `json:"department"`
		Disabled   *bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if in.Role != nil && !roles.ValidRole(*in.Role) {
		writeErr(w, http.StatusBadRequest, "unknown role")
		return
	}
	var target *store.User
	a.st.Read(func(st *store.State) {
		if t, ok := st.Users[id]; ok {
			cp := *t
			target = &cp
		}
	})
	if target == nil {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	if in.Role != nil && target.ID == u.ID {
		writeErr(w, http.StatusBadRequest, "cannot change your own role")
		return
	}
	if in.Disabled != nil && *in.Disabled && target.ID == u.ID {
		writeErr(w, http.StatusBadRequest, "cannot disable yourself")
		return
	}
	// Would this leave zero enabled admins? Only an enabled admin
	// counts as the last one: demoting an already-disabled admin is
	// harmless.
	if !target.Disabled && ((in.Role != nil && target.Role == roles.Admin && *in.Role != roles.Admin) ||
		(in.Disabled != nil && *in.Disabled && target.Role == roles.Admin)) {
		admins := 0
		a.st.Read(func(st *store.State) {
			for _, usr := range st.Users {
				if usr.Role == roles.Admin && !usr.Disabled {
					admins++
				}
			}
		})
		if admins <= 1 {
			writeErr(w, http.StatusBadRequest, "cannot remove the last enabled admin")
			return
		}
	}
	oldRole, oldDisabled := target.Role, target.Disabled
	changed := false
	a.st.WithLock(func(st *store.State) {
		t := st.Users[id]
		if t == nil {
			return
		}
		if in.Role != nil && t.Role != *in.Role {
			t.Role = *in.Role
			t.RoleSource = "local"
			changed = true
		}
		if in.Department != nil && t.Department != *in.Department {
			t.Department = *in.Department
			changed = true
		}
		if in.Disabled != nil && t.Disabled != *in.Disabled {
			t.Disabled = *in.Disabled
			changed = true
		}
		cp := *t
		target = &cp
	})
	if changed {
		if target.Disabled && !oldDisabled {
			a.auth.RevokeUserSessions(target.ID)
		}
		detail := map[string]string{"target": target.Username}
		if oldRole != target.Role {
			detail["role_from"] = oldRole
			detail["role_to"] = target.Role
		}
		if oldDisabled != target.Disabled {
			if target.Disabled {
				detail["disabled"] = "true"
			} else {
				detail["disabled"] = "false"
			}
		}
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin",
			Action: "user_updated", Decision: "allow", Detail: detail})
	}
	_ = a.st.Save()
	writeJSON(w, http.StatusOK, toUserView(target))
}
