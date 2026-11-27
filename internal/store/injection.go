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
}
