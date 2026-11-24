package api

import (
	"net/http"

	"github.com/myjnathan/agentgate/internal/channelpreset"
	"github.com/myjnathan/agentgate/internal/store"
)

// GET /api/channels/presets — the built-in vendor preset library
// (backlog F35 P1). No secrets are involved (base URLs and model
// names are public facts), so the response is the whole list; the
// console uses it for both the channels page and the onboarding
// wizard, replacing the wizard's old hardcoded vendor map.
func (a *API) listChannelPresets(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"presets": channelpreset.Presets()})
}
