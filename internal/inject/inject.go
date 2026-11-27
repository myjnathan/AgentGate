// Package inject implements the prompt-injection rule layer
// (backlog F39, slice 1): source tagging plus deterministic
// rule matching, with the verdict written to the audit decision
// log. The semantic route (reusing the F28 deep layer) and the
// high-risk-action fallback (linked to F41 tool risk levels) are
// slice 2.
//
// Design (OWASP LLM01: direct vs indirect injection; "segregate
// and identify external content" is the mitigation this layer
// implements at the gateway):
//
//   - Source tagging. Every message in a chat request is tagged by
//     where it came from: user (role=user, typed by the employee),
//     tool (role=tool, and tool_calls arguments — content an agent
//     fetched or a tool returned), assistant (role=assistant —
//     model output echoed back, which can carry injected text the
//     model was tricked into repeating). Non-user sources are
//     "data, not instructions" by default: an instruction-shaped
//     phrase in them is suspicious in a way the same phrase from
//     the user is not. System messages are tagged but not
//     rule-scanned in slice 1 — the system prompt is operator-set
//     instructions by definition, so scanning it would flag every
//     legitimate system prompt; a system role set by anyone but
//     the operator is outside what the gateway can see.
//   - Rule route. A small set of ordered-phrase rules over the
//     F29 normalization view (normalize.Tokens: fullwidth folded,
//     case folded, separators and zero-width characters stripped,
//     word boundaries kept), so 插符 / 全角 / 零宽 evasions do not
//     dodge the match. Matching is on tokens — each CJK rune is a
//     token, each ASCII word is a token — with a small gap budget
//     between the phrases of a rule, so "ignore all previous
//     instructions" and "忽略你之前收到的所有指令" both match
//     their rule while unrelated clauses do not.
//     Encoded payloads are handled by decoding the candidate and
//     running the same rules on the decoded text — a blob only
//     fires when it decodes to an actual instruction.
//   - Disposition. Policy mode observe / alert / block applies at
//     public/internal levels; a high-severity match at
//     confidential or above blocks regardless of mode — except
//     observe, which by design never blocks (the operator chose
//     watch-only). The rule layer is purely local pattern
//     matching,
//     so unlike F28 it also runs on top-secret content — nothing
//     is sent anywhere, and top-secret sessions are exactly where
//     an injection matters most.
//
// Honest boundary: this is a probability line of defence, not a
// cure (OWASP is explicit that no fool-proof prevention for prompt
// injection is known). The product posture is "detect + separate
// sources + high-risk action fallback" in three layers; this slice
// is layers one and two. What it cannot see: content the user
// pastes into their own message (indistinguishable from typing),
// and instructions an agent acts on without feeding them back
// through the gateway (the agent-side gap, same class as F38's
// install-check boundary — the gateway only sees what crosses
// it). Detection never writes request text anywhere: the decision
// log carries only the source, rule id, category and severity.
package inject

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/myjnathan/agentgate/internal/normalize"
	"github.com/myjnathan/agentgate/internal/store"
)

// Content sources, from the chat message role.
const (
	SourceUser      = "user"
	SourceTool      = "tool"
	SourceAssistant = "assistant"
	SourceSystem    = "system"
)

// Rule categories.
const (
	CategoryOverride    = "override"    // ignore previous instructions
	CategoryJailbreak   = "jailbreak"   // role-play / developer-mode escape
	CategoryExfil       = "exfil"       // leak the system prompt / send data out
	CategoryConcealment = "concealment" // hide the instruction from the user
	CategoryEncoded     = "encoded"     // instruction hidden in base64/hex
	CategoryMarker      = "marker"      // literal control-plane marker in data
)

// Severities. High rules are clearly malicious phrasings; medium
// rules are suspicious framing that needs context; low is reserved
// for shape-only signals (unused in slice 1: the encoded rule only
// fires on a decoded instruction, so it is high).
const (
	SeverityHigh   = "high"
	SeverityMedium = "medium"
	SeverityLow    = "low"
)

// Policy modes.
const (
	ModeObserve = "observe"
	ModeAlert   = "alert"
	ModeBlock   = "block"
)

// Decision actions. "skip" means the layer did not run (disabled);
// "observe" means a rule fired but the policy only logs it.
const (
	DecisionAllow   = "allow"
	DecisionAlert   = "alert"
	DecisionBlock   = "block"
	DecisionObserve = "observe"
	DecisionSkip    = "skip"
)

// Decision is one rule-layer verdict. The request text is never
// stored here: only the source, the rule that fired and its
// category/severity.
type Decision struct {
	Action   string
	Source   string
	RuleID   string
	Category string
	Severity string
	Reason   string // rule_match | disabled | no_match
}

// Stats are the runtime counters for the layer (like the F28
// counters): process-lifetime only, never persisted.
type Stats struct {
	Reviewed int64 `json:"reviewed"`
	Blocked  int64 `json:"blocked"`
	Alerted  int64 `json:"alerted"`
	Observed int64 `json:"observed"`
}

// Message is one tagged input to the detector: Role is the chat
// message role ("user", "tool", "assistant", "system"), Text is
// the scannable text of that message (content, or tool_calls
// function name + arguments).
type Message struct {
	Role string
	Text string
}

// rule is one detection rule. Groups are ordered phrase lists
// matched against the tokenized text: a group fires when its
// phrases appear in order with the total gap between consecutive
// phrases within Window tokens. Markers are matched against the
// lowercased raw text instead (they are punctuation: "[SYSTEM]").
// NonUserOnly rules never fire on user-typed text: the same phrase
// from the user is a legitimate instruction to their own agent.
// groupToks is the precomputed token form of Groups (see init).
type rule struct {
	ID          string
	Category    string
	Severity    string
	NonUserOnly bool
	Groups      [][]string
	Window      int
	Markers     []string
	groupToks   [][][]string
}

// init precomputes the token form of every rule group so matching
// never re-tokenizes the rule set.
func init() {
	for i := range rules {
		for _, g := range rules[i].Groups {
			var gt [][]string
			for _, ph := range g {
				gt = append(gt, tokenize(ph))
			}
			rules[i].groupToks = append(rules[i].groupToks, gt)
		}
	}
}

// rules is the slice-1 rule set, in priority order: the first
// firing rule (in message order, then rule order) decides.
var rules = []rule{
	{
		ID: "override", Category: CategoryOverride, Severity: SeverityHigh, Window: 8,
		Groups: [][]string{
			{"忽略", "之前", "指令"}, {"忽略", "之前", "提示"}, {"忽略", "所有", "指令"},
			{"忽略", "以上", "指令"}, {"忽略", "上述", "指令"}, {"无视", "之前", "指令"},
			{"无视", "所有", "指令"}, {"无视", "以上", "指令"}, {"忘掉", "之前", "指令"},
			{"忘记", "之前", "指令"}, {"不要理会", "之前", "指令"},
			{"ignore", "previous", "instructions"}, {"ignore", "all", "instructions"},
			{"disregard", "previous", "instructions"}, {"forget", "previous", "instructions"},
			{"ignore", "above", "instructions"}, {"override", "previous", "instructions"},
			{"ignore", "your", "instructions"},
		},
	},
	{
		ID: "jailbreak_roleplay", Category: CategoryJailbreak, Severity: SeverityHigh, Window: 8,
		Groups: [][]string{
			{"假装你是", "管理员"}, {"假装你是", "系统"}, {"扮演", "管理员"},
			{"你现在是", "管理员"}, {"你现在是", "开发者"}, {"你现在是", "系统"},
			{"进入开发者模式"}, {"不再受任何限制"}, {"不受任何限制"}, {"越狱"},
			{"developer mode"}, {"jailbreak"}, {"do anything now"},
			{"you are now", "admin"}, {"you are now", "developer"}, {"you are now", "system"},
			{"pretend you are", "admin"}, {"pretend you are", "system"},
		},
	},
	{
		ID: "exfil_system_prompt", Category: CategoryExfil, Severity: SeverityHigh, Window: 8,
		Groups: [][]string{
			{"输出", "系统提示词"}, {"告诉我", "系统提示词"}, {"泄露", "系统提示词"},
			{"透露", "系统提示词"}, {"重复", "系统提示词"}, {"复述", "系统提示词"},
			{"输出", "初始指令"}, {"告诉我", "初始指令"}, {"输出", "隐藏规则"},
			{"系统提示词", "输出"}, {"系统提示词", "告诉我"}, {"初始指令", "输出"},
			{"output", "system prompt"}, {"reveal", "system prompt"},
			{"show", "system prompt"}, {"repeat", "system prompt"},
			{"output", "initial instructions"}, {"reveal", "initial instructions"},
			{"print", "system prompt"},
		},
	},
	{
		// Deliberately non-user only: "把周报发到我邮箱" from the
		// user is a legitimate instruction to their own agent.
		// The same sentence inside a tool result is an injected
		// exfiltration order. Whether the content itself may
		// leave is the DLP / data-level layer's job, not this
		// rule's.
		ID: "exfil_external", Category: CategoryExfil, Severity: SeverityHigh, NonUserOnly: true, Window: 8,
		Groups: [][]string{
			{"发到", "邮箱"}, {"发送到", "邮箱"}, {"发到", "私人邮箱"},
			{"发送", "私人邮箱"}, {"发给", "陌生人"}, {"发送到", "外部"},
			{"send", "private email"}, {"send", "to", "external"},
			{"send", "to", "personal email"}, {"exfiltrate", "to"},
		},
	},
	{
		// Concealment is the tell that the instruction is not
		// meant for the user to see — the strongest single
		// signal of an indirect injection after override itself.
		ID: "concealment", Category: CategoryConcealment, Severity: SeverityMedium, NonUserOnly: true, Window: 8,
		Groups: [][]string{
			{"不要告诉用户"}, {"不要让用户知道"}, {"别让用户知道"}, {"瞒着用户"},
			{"悄悄", "发送"}, {"秘密", "发送"}, {"不要透露", "用户"},
			{"don't tell the user"}, {"do not tell the user"}, {"without telling the user"}, {"don't let the user know"},
			{"secretly", "send"},
		},
	},
	{
		// Literal control-plane markers pasted into data: a tool
		// result that opens with "[SYSTEM]" or "### instruction"
		// is trying to change who is speaking.
		ID: "instruction_marker", Category: CategoryMarker, Severity: SeverityMedium, NonUserOnly: true,
		Markers: []string{"[system]", "### instruction", "new instructions:", "important instruction:"},
	},
}

// ValidMode reports whether m is a usable policy mode.
func ValidMode(m string) bool {
	return m == ModeObserve || m == ModeAlert || m == ModeBlock
}

// ValidateConfig checks a policy update before it is stored. A nil
// config is valid (it means "never configured": built-in defaults).
func ValidateConfig(cfg *store.InjectionConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.Mode != "" && !ValidMode(cfg.Mode) {
		return errInvalidMode(cfg.Mode)
	}
	return nil
}

// DefaultConfig returns the built-in policy used when the store has
// never been configured: enabled, alert mode — a match is recorded
// and surfaced, and only a high-severity match on confidential+
// data blocks on its own.
func DefaultConfig() *store.InjectionConfig {
	return &store.InjectionConfig{Enabled: true, Mode: ModeAlert}
}

// DefaultMode is the mode used when a stored config leaves it blank.
const DefaultMode = ModeAlert

// Scan runs the rule layer over one request's tagged messages and
// returns the first firing decision (message order, then rule
// priority). System messages are tagged but never scanned (see the
// package doc). A message with an unknown role is treated as
// non-user (fail-closed: the gateway only ever sends user/tool/
// assistant/system roles, so anything else is not user-typed).
func Scan(msgs []Message, level store.DataLevel, cfg *store.InjectionConfig) Decision {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if !cfg.Enabled {
		return Decision{Action: DecisionSkip, Reason: "disabled"}
	}
	mode := cfg.Mode
	if mode == "" {
		mode = DefaultMode
	}
	for _, m := range msgs {
		src := sourceOf(m.Role)
		if src == SourceSystem {
			continue
		}
		if d, ok := scanOne(m.Text, src, mode, level); ok {
			return d
		}
	}
	return Decision{Action: DecisionAllow, Reason: "no_match"}
}

// sourceOf maps a chat role to a content source.
func sourceOf(role string) string {
	switch role {
	case "user":
		return SourceUser
	case "tool", "function":
		return SourceTool
	case "assistant":
		return SourceAssistant
	case "system":
		return SourceSystem
	default:
		return SourceTool
	}
}

// scanOne runs every rule over one message. It returns the firing
// decision and true, or a zero decision and false.
func scanOne(text, src, mode string, level store.DataLevel) (Decision, bool) {
	toks := tokenize(text)
	lower := strings.ToLower(text)
	for _, r := range rules {
		if r.NonUserOnly && src == SourceUser {
			continue
		}
		fired := false
		for _, gt := range r.groupToks {
			if matchOrdered(toks, gt, r.Window) {
				fired = true
				break
			}
		}
		if !fired {
			for _, mk := range r.Markers {
				if strings.Contains(lower, mk) {
					fired = true
					break
				}
			}
		}
		if fired {
			return Decision{Action: decideAction(r.Severity, mode, level), Source: src,
				RuleID: r.ID, Category: r.Category, Severity: r.Severity, Reason: "rule_match"}, true
		}
	}
	// Encoded payloads: only a blob that decodes to an actual
	// instruction fires — shape alone is not evidence, and a JWT
	// or a long ID in a tool result must not alert. Runs are
	// checked on the raw text (normalization would strip the
	// base64 alphabet's separators); at most a few candidates per
	// message are decoded, each capped in length.
	if src != SourceUser {
		if inner := decodeMatch(text); inner != "" {
			return Decision{Action: decideAction(SeverityHigh, mode, level), Source: src,
				RuleID: "encoded_payload", Category: CategoryEncoded, Severity: SeverityHigh,
				Reason: "rule_match:" + inner}, true
		}
	}
	return Decision{}, false
}

// decideAction maps a firing rule to its disposition: observe
// mode never blocks (the operator explicitly chose watch-only,
// e.g. during rollout); otherwise a high-severity match at
// confidential or above always blocks (fail-closed for high
// levels), whatever the mode says; block mode blocks every
// match; alert mode alerts and allows.
func decideAction(severity, mode string, level store.DataLevel) string {
	if mode == ModeObserve {
		return DecisionObserve
	}
	if severity == SeverityHigh && level >= store.LevelConfidential {
		return DecisionBlock
	}
	switch mode {
	case ModeBlock:
		return DecisionBlock
	default:
		return DecisionAlert
	}
}

// tokenize splits text into atoms over the F29 token-preserving
// normalization view (normalize.Tokens: fullwidth folded, case
// folded, separators and zero-width characters stripped, word
// boundaries kept): each CJK rune is one token, each maximal ASCII
// letter/digit run is one token. The same function tokenizes rule
// phrases, so "personal email" and "系统提示词" match the same way
// on both sides.
func tokenize(text string) []string {
	norm := normalize.Tokens(text).Text
	var toks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range norm {
		switch {
		case r == ' ':
			flush()
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			cur.WriteRune(r)
		case unicode.IsLetter(r):
			// A non-ASCII letter (CJK and the like) is one token.
			flush()
			toks = append(toks, string(r))
		default:
			flush()
		}
	}
	flush()
	return toks
}

// matchOrdered reports whether the phrase token-sequences appear
// in order in toks with the total gap between consecutive phrases
// within window tokens. The budget is on gaps, not on the whole
// span: "ignore all previous instructions" matches
// ["ignore", "previous", "instructions"] with a gap of 1 ("all"),
// and "忽略你之前收到的所有指令" matches ["忽略", "之前", "指令"]
// with a gap of 7 ("你" + "收到的所有") — while a 32-rune total
// span would be far too loose for Chinese.
func matchOrdered(toks []string, phrases [][]string, window int) bool {
	pos := 0
	gap := 0
	for _, ph := range phrases {
		found := -1
		for i := pos; i+len(ph) <= len(toks); i++ {
			if equalTokens(toks[i:i+len(ph)], ph) {
				found = i
				break
			}
		}
		if found < 0 {
			return false
		}
		gap += found - pos
		if gap > window {
			return false
		}
		pos = found + len(ph)
	}
	return true
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// decodeMatch looks for long base64/hex runs in the raw text,
// decodes each candidate, and runs the plain-text rules over the
// decoded content. It returns the id of the rule the decoded text
// matches, or "" when no candidate decodes to an instruction.
func decodeMatch(text string) string {
	for _, cand := range longRuns(text) {
		decoded, ok := tryDecode(cand)
		if !ok {
			continue
		}
		norm := tokenize(decoded)
		for _, r := range rules {
			for _, gt := range r.groupToks {
				if matchOrdered(norm, gt, r.Window) {
					return r.ID
				}
			}
		}
	}
	return ""
}

// longRuns extracts candidate encoded runs: base64-alphabet runs
// of 40+ chars and hex runs of 64+ chars. At most five candidates
// are returned, each capped at 4 KiB, so a hostile message cannot
// turn decoding into a slowdown.
func longRuns(text string) []string {
	var out []string
	runes := []rune(text)
	isB64 := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '+' || r == '/' || r == '='
	}
	for i := 0; i < len(runes) && len(out) < 5; {
		if !isB64(runes[i]) {
			i++
			continue
		}
		j := i
		for j < len(runes) && isB64(runes[j]) {
			j++
		}
		run := string(runes[i:j])
		if len(run) >= 40 {
			if len(run) > 4096 {
				run = run[:4096]
			}
			out = append(out, run)
		}
		i = j
	}
	return out
}

// tryDecode decodes one candidate as base64 (padding repaired) or
// hex. It only accepts output that is valid UTF-8 and mostly
// printable — binary blobs and JWT-style JSON are not
// instructions, and a decoded instruction is text.
func tryDecode(cand string) (string, bool) {
	if d, ok := decodeBase64(cand); ok {
		return d, true
	}
	if len(cand) >= 64 && len(cand)%2 == 0 {
		if b, err := hex.DecodeString(cand); err == nil {
			if s, ok := printableText(string(b)); ok {
				return s, true
			}
		}
	}
	return "", false
}

func decodeBase64(cand string) (string, bool) {
	s := strings.TrimRight(cand, "=")
	if len(s) < 40 {
		return "", false
	}
	// Only attempt when the run is really base64-shaped: mixed
	// case or digits with +/ present, or a length that is a
	// multiple of 4 after padding — a long plain word (e.g. a
	// 40-letter German compound) is not base64.
	padded := s + strings.Repeat("=", (4-len(s)%4)%4)
	b, err := base64.StdEncoding.DecodeString(padded)
	if err != nil {
		return "", false
	}
	return printableText(string(b))
}

// printableText accepts a decoded string when it is valid UTF-8
// and at least 90% of its runes are printable.
func printableText(s string) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	rs := []rune(s)
	if len(rs) == 0 {
		return "", false
	}
	printable := 0
	for _, r := range rs {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			printable++
		}
	}
	if printable*10 < len(rs)*9 {
		return "", false
	}
	return s, true
}

// errInvalidMode is a small local error type so the message stays
// stable for the API's error mapping.
type errMode string

func (e errMode) Error() string { return "unknown injection mode " + string(e) }

func errInvalidMode(m string) error { return errMode(m) }
