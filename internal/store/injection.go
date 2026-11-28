package store

// InjectionConfig is the prompt-injection policy (backlog F39),
// stored as one record on the state. Nil means never configured:
// the gateway falls back to the built-in defaults in
// internal/inject (enabled, alert mode) instead of an empty policy.
type InjectionConfig struct {
	// Enabled switches the whole layer. Off means the gateway
	// behaves exactly as before F39.
	Enabled bool `json:"enabled"`
	// Mode is the disposition for a rule match at public/internal
	// levels: "observe" (log only, never blocks), "alert" (alert
	// and allow) or "block". At confidential and above, a
	// high-severity match blocks regardless of mode — except
	// observe, which by design never blocks (fail-closed for high
	// levels, the same convention as the F28 deep-layer fallback).
	Mode string `json:"mode"`
	// SemanticEnabled switches the semantic route (slice 2: local
	// sample matching over an injection sample library plus the
	// F28 deep guard-model endpoint) on or off, independently of
	// the rule layer. Nil means never configured: the route
	// defaults to on. A plain bool cannot express that — a policy
	// stored by slice 1 has no such field and must not silently
	// disable the route when it is read back.
	SemanticEnabled *bool `json:"semantic_enabled,omitempty"`
	// SemanticThreshold is the sample similarity at which
	// non-user text is treated as matching an injection sample.
	// 0 means the built-in default (0.35, shared with F28).
	SemanticThreshold float64 `json:"semantic_threshold"`
}
