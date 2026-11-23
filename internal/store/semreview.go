package store

import "time"

// SemanticReviewSample is one risk sample in the semantic-review
// library (backlog F28): a known risky phrasing (jailbreak /
// inducement / disguise / split exfiltration) that the local
// sample-matching layer compares each request against. The sample
// text itself is an admin-curated risk pattern, not user data; it is
// never written to the audit log (only the sample ID, category and
// similarity are).
type SemanticReviewSample struct {
	ID        string    `json:"id"`
	Category  string    `json:"category"` // jailbreak | inducement | disguise | split_exfil
	Text      string    `json:"text"`
	Action    string    `json:"action"` // block | alert
	CreatedAt time.Time `json:"created_at"`
}

// SemanticReviewConfig is the semantic-review policy (backlog F28),
// stored as one record on the state. Nil on the state means "never
// configured": the gateway falls back to the built-in defaults in
// internal/semreview (enabled, conservative threshold, local sample
// matching only, no deep layer). The distinction matters: an admin
// who deletes every sample has configured an empty library, which
// must not silently resurrect the seed samples.
type SemanticReviewConfig struct {
	// Enabled switches the whole layer. Off means the gateway
	// behaves exactly as before F28 (red line ③).
	Enabled bool `json:"enabled"`
	// Threshold is the sample-matching similarity (0..1) at or
	// above which a request is treated as matching the sample.
	Threshold float64 `json:"threshold"`
	// TimeoutMS caps one deep-layer review call. On timeout the
	// conservative rule applies: confidential+ blocks, lower
	// levels alert and allow (red line ④).
	TimeoutMS int `json:"timeout_ms"`
	// ControversialAction is what a deep-layer "Controversial"
	// verdict does: "alert" (default) or "block". An "Unsafe"
	// verdict always blocks.
	ControversialAction string `json:"controversial_action"`
	// DeepEnabled turns on the optional second layer: a
	// Qwen3Guard-style self-hosted guard model over an
	// OpenAI-compatible endpoint. Off by default; the local
	// sample-matching layer is the default layer.
	DeepEnabled bool `json:"deep_enabled"`
	// DeepChannelID names the store channel that serves the guard
	// model; the review call reuses that channel's base URL and
	// keys, and its trust type gates the call (red line ①).
	DeepChannelID string `json:"deep_channel_id"`
	// DeepModel is the guard model name on that channel (e.g.
	// "Qwen/Qwen3Guard-Gen-4B").
	DeepModel string `json:"deep_model"`
	// Samples is the risk-sample library the default layer matches
	// against.
	Samples []SemanticReviewSample `json:"samples"`
}
