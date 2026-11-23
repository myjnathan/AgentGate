package store

import "time"

// AutoRouteSample is one complexity sample in the smart-routing
// library (backlog F30): a request phrasing an admin (or the seed)
// labels simple or complex. The classifier compares each
// model=auto request against the library with the same
// deterministic n-gram vectorizer F28 uses (internal/semreview),
// so the two layers share one "embedding" infrastructure. The
// sample text is an admin-curated example, not user data; it is
// never written to the audit log (only the sample ID, tier and
// similarity are).
type AutoRouteSample struct {
	ID        string    `json:"id"`
	Tier      string    `json:"tier"` // simple | complex
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// AutoRouteConfig is the smart-routing policy (backlog F30), stored
// as one record on the state. Nil on the state means "never
// configured": the gateway falls back to the built-in defaults in
// internal/autorouter (enabled, threshold 0.35, simple group
// mock-eco, complex group mock-pro, seed samples). The distinction
// matters: an admin who deletes every sample has configured an
// empty library, which must not silently resurrect the seed samples.
type AutoRouteConfig struct {
	// Enabled switches the whole layer. Off means model=auto is
	// rejected as unavailable — there is no model named "auto" on
	// any channel, and silently falling back to a default model
	// would hide a misconfiguration.
	Enabled bool `json:"enabled"`
	// Threshold is the sample-matching similarity (0..1) at or
	// above which a request is treated as matching the sample.
	Threshold float64 `json:"threshold"`
	// SimpleGroup is the ordered list of model names for simple
	// requests (the cheap model group). The order is the
	// selection priority, like 1Panel's model groups: the first
	// model with a compliant channel serves the request.
	SimpleGroup []string `json:"simple_group"`
	// ComplexGroup is the ordered list of model names for complex
	// requests (the strong model group). Same priority semantics.
	ComplexGroup []string `json:"complex_group"`
	// Samples is the complexity-sample library the classifier
	// matches against.
	Samples []AutoRouteSample `json:"samples"`
}
