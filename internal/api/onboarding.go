// Package api — first-run onboarding (backlog F34).
//
//   - POST /api/setup (+ GET /api/setup/status): the one-time setup
//     code printed to the server log on first boot creates the first
//     administrator (dsh-ee style). The code itself is never stored —
//     only its SHA-256 hash — and it is single-use: after five failed
//     attempts it is void and a restart issues a new one.
//   - GET /api/onboarding/templates + POST /api/onboarding/apply-template:
//     industry templates (general conservative / medical device) apply
//     the "保守模式" preset package: launch phase observe, both tiers
//     redacted, weekly report cadence declared, template words added
//     to the gateway detector.
//   - GET/POST /api/onboarding/launch-phase: the runtime rollout
//     phase (observe | warn | block), persisted in the store and
//     applied to the gateway without a restart.
//   - GET /api/onboarding/checklist: the onboarding green-light list
//     for the dashboard and the wizard's final step.
//   - POST /api/onboarding/ack + POST /api/onboarding/complete: the
//     manual master-key-backup ack and the wizard completion marker.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/onboard"
	"github.com/myjnathan/agentgate/internal/store"
)

// maxSetupAttempts caps setup-code guesses: the code is 12 characters
// from a 32-letter alphabet (60 bits), so five wrong guesses void it
// rather than letting an attacker grind through the space. A restart
// issues a fresh code.
const maxSetupAttempts = 5

// SetSetupCode stores the SHA-256 hash of the one-time setup code
// issued at first boot (F34). The plaintext code is only ever in the
// server log; the API keeps the hash and forgets it after use.
func (a *API) SetSetupCode(code string) {
	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	a.setupCodeHash = onboard.SetupCodeHash(code)
	a.setupAttempts = 0
}

// SetLaunchPhaseApplier wires the gateway's runtime phase setter
// (F34). Unset means phase changes are persisted but not applied to
// a live gateway (tests that build the API without a gateway).
func (a *API) SetLaunchPhaseApplier(fn func(string)) { a.launchPhaseApplier = fn }

// SetRedactForTierApplier wires the gateway's runtime redaction
// setter (F34 conservative preset).
func (a *API) SetRedactForTierApplier(fn func(map[store.Tier]bool)) {
	a.redactForTierApplier = fn
}

// SetWordsApplier wires the gateway's runtime word-list setter
// (F34 template words).
func (a *API) SetWordsApplier(fn func(banned, custom []string)) { a.wordsApplier = fn }

// SetWordsReader wires the gateway's current word lists (F34): the
// template apply unions onto them instead of replacing them.
func (a *API) SetWordsReader(fn func() (banned, custom []string)) { a.wordsReader = fn }

func (a *API) userCount() int {
	n := 0
	a.st.Read(func(st *store.State) { n = len(st.Users) })
	return n
}

func (a *API) onboardingState() *store.OnboardingState {
	var ob *store.OnboardingState
	a.st.Read(func(st *store.State) { ob = st.Onboarding })
	if ob == nil {
		return &store.OnboardingState{}
	}
	return ob
}

// GET /api/setup/status — whether the first admin still needs to be
// created. Unauthenticated: the console needs it before login to
// decide between the login form and the setup form.
func (a *API) setupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"setup_required": a.userCount() == 0})
}

// POST /api/setup — create the first administrator with the
// one-time setup code. Unauthenticated by design (there is no admin
// yet to authenticate as); the code is the credential.
func (a *API) setupFirstAdmin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code     string `json:"code"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if a.userCount() > 0 {
		writeErr(w, http.StatusConflict, "setup already complete")
		return
	}
	a.setupMu.Lock()
	hash := a.setupCodeHash
	attempts := a.setupAttempts
	a.setupMu.Unlock()
	// Attempts are checked before the hash: after the fifth failure
	// the hash is cleared, and the answer must still be "exhausted"
	// (410), not "no code issued" (503).
	if attempts >= maxSetupAttempts {
		writeErr(w, http.StatusGone, "setup code exhausted after too many failed attempts (restart the server to issue a new one)")
		return
	}
	if hash == nil {
		writeErr(w, http.StatusServiceUnavailable, "no setup code issued (restart the server to issue one)")
		return
	}
	if subtle.ConstantTimeCompare(onboard.SetupCodeHash(in.Code), hash) != 1 {
		a.setupMu.Lock()
		a.setupAttempts++
		exhausted := a.setupAttempts >= maxSetupAttempts
		a.setupMu.Unlock()
		if exhausted {
			a.setupMu.Lock()
			a.setupCodeHash = nil
			a.setupMu.Unlock()
			a.audit.Append(audit.Event{Actor: "system", Kind: "auth", Action: "setup_code_exhausted", Decision: "block"})
		}
		writeErr(w, http.StatusUnauthorized, "invalid setup code")
		return
	}
	if !validSetupUsername(in.Username) {
		writeErr(w, http.StatusBadRequest, "username must be 3-32 letters, digits, dot, underscore or hyphen")
		return
	}
	if len(in.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	var taken bool
	a.st.Read(func(st *store.State) {
		for _, u := range st.Users {
			if u.Username == in.Username {
				taken = true
			}
		}
	})
	if taken {
		writeErr(w, http.StatusConflict, "username already exists")
		return
	}
	h, err := auth.HashPassword(in.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash failed")
		return
	}
	u := &store.User{ID: store.NewID("usr"), Username: in.Username, DisplayName: in.Username,
		PasswordHash: h, Department: "IT", Role: "admin", AuthSource: "local"}
	a.st.WithLock(func(st *store.State) { st.Users[u.ID] = u })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save failed")
		return
	}
	a.setupMu.Lock()
	a.setupCodeHash = nil
	a.setupAttempts = 0
	a.setupMu.Unlock()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "auth", Action: "first_admin_created",
		Decision: "allow", Detail: map[string]string{"role": "admin"}})
	writeJSON(w, http.StatusCreated, map[string]any{"username": u.Username, "role": u.Role})
}

func validSetupUsername(s string) bool {
	if len(s) < 3 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// GET /api/onboarding/templates — the built-in industry templates
// and which one (if any) is applied.
func (a *API) listTemplates(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"templates":            onboard.Templates(),
		"applied_template":     a.onboardingState().TemplateID,
		"conservative_applied": a.onboardingState().ConservativeApplied,
	})
}

// POST /api/onboarding/apply-template — apply an industry template:
// the conservative preset (phase observe, both tiers redacted,
// weekly report cadence) plus the template's word list. The phase
// and words are applied to the live gateway; the choice is
// persisted so a restart re-applies the words.
func (a *API) applyTemplate(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		TemplateID string `json:"template_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.TemplateID == "" {
		writeErr(w, http.StatusBadRequest, "template_id required")
		return
	}
	tmpl, ok := onboard.TemplateByID(in.TemplateID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown template")
		return
	}
	// added counts the template words newly recorded: the same
	// words are unioned onto the live gateway when it is wired,
	// and re-applied from the persisted list on the next boot.
	ob := a.onboardingState()
	var added int
	ob.AppliedCustomWords, added = unionWords(ob.AppliedCustomWords, tmpl.CustomWords)
	// F35: if the DLP lists are already console-managed, the
	// template words must join the managed copy too — otherwise
	// the next restart would drop them when the managed copy
	// overrides the boot lists.
	managedDLP := a.unionTemplateWords(tmpl.CustomWords)
	if a.wordsReader != nil && a.wordsApplier != nil {
		banned, custom := a.wordsReader()
		merged, _ := unionWords(custom, tmpl.CustomWords)
		a.wordsApplier(banned, merged)
	}
	if a.redactForTierApplier != nil {
		m := map[store.Tier]bool{}
		for _, t := range onboard.Conservative.RedactTiers {
			m[store.Tier(t)] = true
		}
		a.redactForTierApplier(m)
	}
	if a.launchPhaseApplier != nil {
		a.launchPhaseApplier(onboard.Conservative.Phase)
	}
	ob.TemplateID = tmpl.ID
	ob.ConservativeApplied = true
	ob.LaunchPhase = onboard.Conservative.Phase
	ob.ReportCadence = onboard.Conservative.ReportCadence
	a.st.WithLock(func(st *store.State) {
		st.Onboarding = ob
		if managedDLP != nil {
			st.DLP = managedDLP
		}
	})
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save failed")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "template_applied",
		Decision: "allow", Detail: map[string]string{"template": tmpl.ID,
			"launch_phase": onboard.Conservative.Phase, "custom_words_added": strconv.Itoa(added)}})
	writeJSON(w, http.StatusOK, map[string]any{
		"template_id": tmpl.ID, "launch_phase": onboard.Conservative.Phase,
		"report_cadence": onboard.Conservative.ReportCadence, "custom_words_added": added})
}

// unionWords merges extra into base (deduplicated, sorted for
// determinism) and reports how many of extra were new.
func unionWords(base, extra []string) ([]string, int) {
	seen := map[string]bool{}
	for _, w := range base {
		seen[w] = true
	}
	added := 0
	out := append([]string(nil), base...)
	for _, w := range extra {
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		added++
	}
	sort.Strings(out)
	return out, added
}

// GET /api/onboarding/launch-phase — the current rollout phase.
func (a *API) getLaunchPhase(w http.ResponseWriter, r *http.Request, u *store.User) {
	phase := a.onboardingState().LaunchPhase
	if phase == "" {
		phase = onboard.PhaseBlock
	}
	writeJSON(w, http.StatusOK, map[string]any{"launch_phase": phase})
}

// POST /api/onboarding/launch-phase — switch the rollout phase at
// runtime. The change is persisted and applied to the live gateway;
// the audit event records the previous phase so an advance to block
// (or a retreat to observe) is always traceable.
func (a *API) setLaunchPhase(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Phase string `json:"phase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	phase, ok := onboard.ParsePhase(in.Phase)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid phase (observe|warn|block)")
		return
	}
	ob := a.onboardingState()
	prev := ob.LaunchPhase
	if prev == "" {
		prev = onboard.PhaseBlock
	}
	ob.LaunchPhase = phase
	a.st.WithLock(func(st *store.State) { st.Onboarding = ob })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save failed")
		return
	}
	if a.launchPhaseApplier != nil {
		a.launchPhaseApplier(phase)
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "launch_phase_changed",
		Decision: "allow", Detail: map[string]string{"phase": phase, "previous_phase": prev}})
	writeJSON(w, http.StatusOK, map[string]any{"launch_phase": phase, "previous_phase": prev})
}

type checklistItem struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// GET /api/onboarding/checklist — the onboarding green-light list:
// each item is computed live from the store, the audit log and the
// folder index, so a green item is a measured fact, not a stored
// claim. would_block_count counts observe-mode calls that would
// have been blocked; warned_count counts warn-phase warnings.
func (a *API) getChecklist(w http.ResponseWriter, r *http.Request, u *store.User) {
	now := time.Now()
	ob := a.onboardingState()
	phase := ob.LaunchPhase
	if phase == "" {
		phase = onboard.PhaseBlock
	}
	var adminOK, chanOK, chanHealthy, deviceOK, kbOK bool
	var chanDetail, deviceDetail, kbDetail string
	a.st.Read(func(st *store.State) {
		for _, usr := range st.Users {
			if !usr.Disabled && (usr.Role == "admin" || usr.Role == "security") {
				adminOK = true
			}
		}
		nEnabled, nHealthy := 0, 0
		for _, ch := range st.Channels {
			if ch.Disabled {
				continue
			}
			nEnabled++
			if ch.Health(now) == "healthy" {
				nHealthy++
			}
		}
		chanOK = nEnabled > 0
		chanHealthy = nHealthy > 0
		chanDetail = strconv.Itoa(nEnabled) + " 个已启用渠道，" + strconv.Itoa(nHealthy) + " 个健康"
		nTokens := 0
		for _, tok := range st.Tokens {
			if !tok.Revoked {
				nTokens++
			}
		}
		deviceOK = nTokens > 0
		deviceDetail = strconv.Itoa(nTokens) + " 个有效令牌"
	})
	if a.fk != nil {
		nDocs := 0
		for _, src := range a.fk.Sources() {
			if src.Enabled {
				nDocs += a.fk.DocCount(src.ID)
			}
		}
		kbOK = nDocs > 0
		kbDetail = strconv.Itoa(nDocs) + " 篇已索引文档"
	}
	events := a.audit.All()
	auditOK := len(events) > 0 && a.audit.Verify()
	wouldBlock, warned := 0, 0
	for _, ev := range events {
		if ev.Kind != "model_call" {
			continue
		}
		if strings.HasPrefix(ev.Decision, "dry_run:") {
			wouldBlock++
		} else if ev.Decision == "warn" {
			warned++
		}
	}
	items := []checklistItem{
		{ID: "admin_created", Label: "管理员已建", OK: adminOK},
		{ID: "channel_configured", Label: "渠道已配置", OK: chanOK, Detail: chanDetail},
		{ID: "channel_healthy", Label: "渠道健康（未冷却、未停用）", OK: chanHealthy, Detail: chanDetail},
		{ID: "device_onboarded", Label: "至少一台设备已接入", OK: deviceOK, Detail: deviceDetail},
		{ID: "kb_indexed", Label: "知识库已索引文件夹", OK: kbOK, Detail: kbDetail},
		{ID: "audit_writing", Label: "审计在写且哈希链完整", OK: auditOK, Detail: strconv.Itoa(len(events)) + " 条事件"},
		{ID: "master_key_backed_up", Label: "主密钥已备份确认", OK: ob.MasterKeyBackedUp, Detail: "人工确认：主密钥丢失将导致全部加密密钥无法恢复"},
		{ID: "launch_phase", Label: "当前上线阶段", OK: true, Detail: phase},
	}
	allOK := true
	for _, it := range items {
		if !it.OK {
			allOK = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"launch_phase": phase, "template_id": ob.TemplateID,
		"report_cadence": ob.ReportCadence, "completed": ob.CompletedAt != nil,
		"completed_at": ob.CompletedAt, "would_block_count": wouldBlock,
		"warned_count": warned, "items": items, "all_ok": allOK,
	})
}

// POST /api/onboarding/ack — the operator's manual ack that the
// master key is backed up (a checklist item no probe can verify).
func (a *API) ackOnboarding(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		MasterKeyBackedUp bool `json:"master_key_backed_up"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	ob := a.onboardingState()
	ob.MasterKeyBackedUp = in.MasterKeyBackedUp
	a.st.WithLock(func(st *store.State) { st.Onboarding = ob })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save failed")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "onboarding_ack",
		Decision: "allow", Detail: map[string]string{"master_key_backed_up": boolStr(in.MasterKeyBackedUp)}})
	writeJSON(w, http.StatusOK, map[string]any{"master_key_backed_up": in.MasterKeyBackedUp})
}

// POST /api/onboarding/complete — mark the first-run wizard done.
// Idempotent: completing twice keeps the first timestamp.
func (a *API) completeOnboarding(w http.ResponseWriter, r *http.Request, u *store.User) {
	ob := a.onboardingState()
	if ob.CompletedAt == nil {
		now := time.Now()
		ob.CompletedAt = &now
		a.st.WithLock(func(st *store.State) { st.Onboarding = ob })
		if err := a.st.Save(); err != nil {
			writeErr(w, http.StatusInternalServerError, "save failed")
			return
		}
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "onboarding_completed",
			Decision: "allow", Detail: map[string]string{"template": ob.TemplateID, "launch_phase": ob.LaunchPhase}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"completed": true, "completed_at": ob.CompletedAt})
}
