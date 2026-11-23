package gateway

import (
	"encoding/json"

	"github.com/myjnathan/agentgate/internal/autorouter"
	"github.com/myjnathan/agentgate/internal/store"
)

// Smart routing (backlog F30) wiring: the gateway resolves
// model=auto to a concrete model before any channel lookup —
// "auto" names no channel. Classification (sample matching +
// rules) runs on the request text in-process; the tier's model
// group is walked in priority order and the first model with a
// compliant candidate channel serves. The candidate rules are
// unchanged (pickCandidates applies data level + trust type +
// cooldown per model), so complexity routing can only choose
// inside the compliant set, never widen it. A tier whose group
// has no compliant channel falls back to the other tier; if
// neither tier has one, the request fails with the same explicit
// errors as a direct model request.

// autoResolution is one model=auto resolution: the concrete model,
// its compliant candidate channels (least-privileged first, as
// pickCandidates returns them), the classifier decision, the tier
// that actually served the request, and whether the served tier
// differs from the classified tier.
type autoResolution struct {
	model        string
	candidates   []*store.Channel
	dec          autorouter.Decision
	servedTier   string
	tierFallback bool
}

// AutoRouteStats returns the runtime counters for the layer.
func (g *Gateway) AutoRouteStats() autorouter.Stats {
	g.autoMu.Lock()
	defer g.autoMu.Unlock()
	return g.autoStats
}

// autoRouteConfig reads the stored policy, falling back to the
// built-in defaults when the admin never configured the layer.
func (g *Gateway) autoRouteConfig() *store.AutoRouteConfig {
	var cfg *store.AutoRouteConfig
	g.st.Read(func(st *store.State) {
		if st.AutoRoute != nil {
			cp := *st.AutoRoute
			cfg = &cp
		}
	})
	if cfg == nil {
		return autorouter.DefaultConfig()
	}
	return cfg
}

// recordAutoRoute counts one resolved auto request.
func (g *Gateway) recordAutoRoute(dec autorouter.Decision, servedTier string, tierFallback bool) {
	g.autoMu.Lock()
	defer g.autoMu.Unlock()
	g.autoStats.Auto++
	if servedTier == autorouter.TierSimple {
		g.autoStats.Simple++
	} else {
		g.autoStats.Complex++
	}
	switch dec.Source {
	case autorouter.SourceSample:
		g.autoStats.Sample++
	case autorouter.SourceRule:
		g.autoStats.Rule++
	}
	if tierFallback {
		g.autoStats.TierFallback++
	}
}

// resolveAutoModel picks the concrete model for a model=auto
// request. It classifies the request, then walks the chosen tier's
// group in priority order and returns the first model with a
// compliant candidate channel. If the chosen tier has no compliant
// channel, it tries the other tier and marks the tier fallback;
// the fallback stays inside the compliant set, so the trust
// boundary never moves. ignoreTrust is only true when a valid
// exception grant waives the trust rule for this one call (F24):
// the same classification is reused, only the channel filter
// changes. It returns an empty model when no tier has a compliant
// channel.
func (g *Gateway) resolveAutoModel(cfg *store.AutoRouteConfig, text string, rounds int, hasToolCalls bool, level store.DataLevel, ignoreTrust bool) autoResolution {
	dec := autorouter.NewClassifier(cfg.Samples).Classify(text, rounds, hasToolCalls, cfg)
	for _, tier := range []string{dec.Tier, otherAutoTier(dec.Tier)} {
		for _, model := range autoGroup(cfg, tier) {
			if cands := g.pickCandidates(model, level, ignoreTrust); len(cands) > 0 {
				return autoResolution{model: model, candidates: cands, dec: dec,
					servedTier: tier, tierFallback: tier != dec.Tier}
			}
		}
	}
	return autoResolution{dec: dec, servedTier: dec.Tier}
}

// autoGroup returns the configured model group for a tier.
func autoGroup(cfg *store.AutoRouteConfig, tier string) []string {
	if tier == autorouter.TierSimple {
		return cfg.SimpleGroup
	}
	return cfg.ComplexGroup
}

// otherAutoTier returns the tier the router falls back to when the
// chosen tier's group has no compliant channel.
func otherAutoTier(tier string) string {
	if tier == autorouter.TierSimple {
		return autorouter.TierComplex
	}
	return autorouter.TierSimple
}

// autoGroupModels returns the union of both model groups
// (deduplicated, simple group first), for the error paths when no
// tier resolves: the gateway reports why no group model could
// serve, using the same checks as a direct model request.
func autoGroupModels(cfg *store.AutoRouteConfig) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range append(append([]string(nil), cfg.SimpleGroup...), cfg.ComplexGroup...) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// autoChannelsCoolingDown reports whether any group model has a
// channel that could serve it at this level but is cooling down.
func (g *Gateway) autoChannelsCoolingDown(cfg *store.AutoRouteConfig, level store.DataLevel, ignoreTrust bool) bool {
	for _, m := range autoGroupModels(cfg) {
		if g.channelsCoolingDown(m, level, ignoreTrust) {
			return true
		}
	}
	return false
}

// autoChannelsBlockedByTrust reports whether any group model has a
// channel that could serve it at this level but is ruled out by its
// trust type.
func (g *Gateway) autoChannelsBlockedByTrust(cfg *store.AutoRouteConfig, level store.DataLevel, ignoreTrust bool) bool {
	for _, m := range autoGroupModels(cfg) {
		if g.channelsBlockedByTrust(m, level, ignoreTrust) {
			return true
		}
	}
	return false
}

// rewriteModel replaces the "model" field of a chat-completions
// request body with the resolved concrete model, preserving every
// other field (same generic-map re-marshal as redactBody).
func rewriteModel(body []byte, model string) []byte {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		return body
	}
	m, err := json.Marshal(model)
	if err != nil {
		return body
	}
	generic["model"] = m
	out, err := json.Marshal(generic)
	if err != nil {
		return body
	}
	return out
}
