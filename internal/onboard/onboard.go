// Package onboard holds the first-run delivery surface (backlog
// F34): launch phases, industry templates and the one-time setup
// code that creates the first administrator.
//
// The package is pure data and validation — it never touches the
// store, the gateway or the network. Applying a template or a phase
// lives in the API layer, which owns the store and the gateway
// setters; this package only says what the options are.
package onboard

import (
	"crypto/rand"
	"crypto/sha256"
	"strings"
)

// Launch phases (blueprint §6.1, three-step rollout):
//
//	observe — DryRun: audit only, nothing blocks or redacts.
//	warn    — remind: classification blocks for the BYOA tier are
//	          waived (the call still only reaches trust-compliant
//	          channels, redacted when the channel demands it) and
//	          recorded with decision "warn"; safety blocks (banned
//	          words, semantic review) and "no compliant channel"
//	          still block.
//	block   — enforce: every rule blocks as configured.
const (
	PhaseObserve = "observe"
	PhaseWarn    = "warn"
	PhaseBlock   = "block"
	// DefaultPhase is what a fresh install starts in: observe first,
	// so a misconfiguration costs nothing on day one. Existing
	// installs keep "block" (their previous behaviour) and demo
	// mode stays "block" (the PoC demonstrates blocks).
	DefaultPhase = PhaseObserve
)

// ValidPhase reports whether p is one of the three launch phases.
func ValidPhase(p string) bool {
	switch p {
	case PhaseObserve, PhaseWarn, PhaseBlock:
		return true
	}
	return false
}

// ParsePhase parses a launch phase string.
func ParsePhase(s string) (string, bool) {
	if ValidPhase(s) {
		return s, true
	}
	return "", false
}

// Preset is the named "conservative mode" package (blueprint §6.1):
// the starting state an install is in after the first-run wizard
// applies a template. Everything here is a combination of engine
// capabilities that already exist — no new governance logic.
type Preset struct {
	// Phase is the launch phase the preset sets.
	Phase string
	// RedactTiers are the token tiers whose PII/credential findings
	// are redacted before forwarding.
	RedactTiers []string
	// ReportCadence is the compliance-report rhythm the preset
	// declares ("weekly"); the report itself is still generated
	// from the reports page — no scheduled generation exists yet.
	ReportCadence string
	// TopSecretExceptions stays false: top-secret calls are never
	// excepted unless the operator explicitly opts in with
	// AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS=1 (F24).
	TopSecretExceptions bool
}

// Conservative is the "保守模式" preset package.
var Conservative = Preset{
	Phase:               PhaseObserve,
	RedactTiers:         []string{"managed", "byoa"},
	ReportCadence:       "weekly",
	TopSecretExceptions: false,
}

// FolderSuggestion is one folder-to-level mapping suggestion a
// template ships: guidance shown in the wizard, not auto-applied
// (the folders themselves only exist on the customer's file server).
type FolderSuggestion struct {
	Pattern string `json:"pattern"` // e.g. "注册资料"
	Level   string `json:"level"`   // public|internal|confidential|secret
}

// Template is one industry template (F34 P0): a preset package plus
// the industry's word list, example departments and folder mapping
// suggestions. The first two ship in this round; more industries are
// data, not code — add another entry here.
type Template struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	NameEN        string             `json:"name_en"`
	Description   string             `json:"description"`
	DescriptionEN string             `json:"description_en"`
	CustomWords   []string           `json:"custom_words"`
	Departments   []string           `json:"departments"`
	FolderHints   []FolderSuggestion `json:"folder_suggestions"`
	ReportCadence string             `json:"report_cadence"`
}

func generalConservative() Template {
	return Template{
		ID:            "general-conservative",
		Name:          "通用保守",
		NameEN:        "General conservative",
		Description:   "默认模板：不上任何行业词表，只用内置 PII 与凭据检测；机密以上默认拦、审计全开、脱敏开，上线阶段默认为观察模式。",
		DescriptionEN: "Default template: no industry word list, built-in PII and credential detection only; confidential and above blocked by default, audit fully on, redaction on, launch phase observe.",
		CustomWords:   []string{},
		Departments:   []string{"行政部", "财务部", "业务部", "管理层"},
		FolderHints: []FolderSuggestion{
			{Pattern: "共享盘根目录", Level: "internal"},
			{Pattern: "人事 / 财务", Level: "confidential"},
		},
		ReportCadence: "weekly",
	}
}

func medicalDevice() Template {
	return Template{
		ID:            "medical-device",
		Name:          "医疗器械",
		NameEN:        "Medical devices",
		Description:   "星河精密种子正式化：医疗器械行业的预置词表（注册证号、临床数据、设计输出等一律按机密管控）与部门结构；其余策略与通用保守模板相同。",
		DescriptionEN: "The Xinghe Precision demo seed, formalised: preset word list for medical devices (registration numbers, clinical data, design outputs are all confidential) and the department structure; all other policy matches the general conservative template.",
		// Custom words are classified confidential by the gateway
		// (F18/F19 word handling): the values themselves are
		// fictional industry terms, never real customer data.
		CustomWords: []string{"注册证号", "临床数据", "设计输出", "不良事件", "XH-200", "星河 XH-200"},
		Departments: []string{"研发部", "注册部", "质量部", "生产部", "销售部"},
		FolderHints: []FolderSuggestion{
			{Pattern: "质量体系文件", Level: "internal"},
			{Pattern: "注册资料", Level: "confidential"},
			{Pattern: "设计输出", Level: "confidential"},
		},
		ReportCadence: "weekly",
	}
}

// Templates returns the built-in industry templates in wizard order.
func Templates() []Template {
	return []Template{generalConservative(), medicalDevice()}
}

// TemplateByID returns the template with the given ID.
func TemplateByID(id string) (Template, bool) {
	for _, t := range Templates() {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// NewSetupCode generates a one-time first-admin setup code
// (backlog F34, dsh-ee style): 12 characters from an unambiguous
// alphabet, grouped XXXX-XXXX-XXXX for typing. Only the SHA-256
// hash is ever stored — the code itself is printed to the server
// log once at first boot and nowhere else.
func NewSetupCode() string {
	const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the process cannot safely issue
		// a code at all; callers treat "" as "no code issued".
		return ""
	}
	var sb strings.Builder
	for i, v := range b {
		if i == 4 || i == 8 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(v)%len(alphabet)])
	}
	return sb.String()
}

// SetupCodeHash returns the SHA-256 hash of a setup code, the only
// form ever stored or compared.
func SetupCodeHash(code string) []byte {
	sum := sha256.Sum256([]byte(code))
	return sum[:]
}
