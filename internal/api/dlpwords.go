package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// DLP word-list management (backlog F35 P1): the console page that
// replaces "edit env vars and restart" for the two word lists the
// gateway detector enforces — banned words (block the call) and
// custom words (enterprise-sensitive, treated as confidential).
// Changes take effect on the next call (the gateway rebuilds its
// detector per request), survive restarts (persisted in the store),
// reach sidecars on their next policy refresh (the snapshot reads
// the gateway's live lists), and are audited without the word
// itself — a word can be a project codename, so it never lands in
// the audit log. The MCP detector is still built once at startup
// (F34 note), so a word change does not reach MCP tools until
// restart; that gap is documented in docs/delivery-p1.md.

const maxDLPWordRunes = 128

// GET /api/dlp/words — the effective lists with each word's origin:
// "manual" (added on this page), "template" (an industry template
// applied it), or "env" (boot env vars / demo seed).
func (a *API) listDLPWords(w http.ResponseWriter, r *http.Request, u *store.User) {
	banned, custom := a.liveWords()
	dlp := a.dlpState()
	ob := a.onboardingState()
	out := map[string]any{
		"banned": wordEntries(banned, dlp.ManualBanned, nil),
		"custom": wordEntries(custom, dlp.ManualCustom, ob.AppliedCustomWords),
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/dlp/words — add or remove one word. The first write
// initialises any unmanaged list from the live gateway lists, so
// env/template words are preserved rather than dropped. The
// response carries counts only, never the word.
func (a *API) updateDLPWords(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		List   string `json:"list"`   // banned | custom
		Action string `json:"action"` // add | remove
		Word   string `json:"word"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.List != "banned" && in.List != "custom" {
		writeErr(w, http.StatusBadRequest, "list must be banned or custom")
		return
	}
	if in.Action != "add" && in.Action != "remove" {
		writeErr(w, http.StatusBadRequest, "action must be add or remove")
		return
	}
	word, ok := validDLPWord(in.Word)
	if !ok {
		writeErr(w, http.StatusBadRequest, "word must be 1-128 characters without control characters")
		return
	}
	banned, custom := a.liveWords()
	var dlp *store.DLPWordLists
	a.st.Read(func(st *store.State) { dlp = st.DLP })
	if dlp == nil {
		dlp = &store.DLPWordLists{}
	}
	dlp.EnsureFrom(banned, custom)
	changed := false
	if in.List == "banned" {
		dlp.Banned, dlp.ManualBanned, changed = mutateWordList(dlp.Banned, dlp.ManualBanned, word, in.Action == "add")
	} else {
		dlp.Custom, dlp.ManualCustom, changed = mutateWordList(dlp.Custom, dlp.ManualCustom, word, in.Action == "add")
	}
	if in.Action == "remove" && !changed {
		writeErr(w, http.StatusBadRequest, "word not in list")
		return
	}
	if a.wordsApplier != nil {
		a.wordsApplier(dlp.Banned, dlp.Custom)
	}
	a.st.WithLock(func(st *store.State) { st.DLP = dlp })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save failed")
		return
	}
	// The word itself is never written to the audit log: it can be
	// a project codename (sensitive original text).
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "dlp_word_changed",
		Decision: "allow", Detail: map[string]string{"list": in.List, "action": in.Action,
			"banned_count": strconv.Itoa(len(dlp.Banned)), "custom_count": strconv.Itoa(len(dlp.Custom))}})
	writeJSON(w, http.StatusOK, map[string]any{"list": in.List, "action": in.Action,
		"changed": changed, "banned_count": len(dlp.Banned), "custom_count": len(dlp.Custom)})
}

// unionTemplateWords merges template words into the managed custom
// list (F34 apply-template) and returns the updated lists, or nil
// when the lists were never console-managed. The caller saves it
// in the same store write as the template choice: without it, a
// template applied after the word lists were first managed would
// lose its words on the next restart, when the managed copy
// overrides the boot lists.
func (a *API) unionTemplateWords(words []string) *store.DLPWordLists {
	var dlp *store.DLPWordLists
	a.st.Read(func(st *store.State) { dlp = st.DLP })
	if dlp == nil || dlp.Custom == nil {
		return nil
	}
	dlp.Custom, _ = unionWords(dlp.Custom, words)
	return dlp
}

// mutateWordList applies one add/remove to a list. Add is
// idempotent (adding an existing word is a no-op success); remove
// of a missing word reports changed=false so the caller can 400.
// Manual origin is tracked alongside the list.
func mutateWordList(list, manual []string, word string, add bool) (out, outManual []string, changed bool) {
	idx := sort.SearchStrings(list, word)
	if add {
		if idx < len(list) && list[idx] == word {
			return list, manual, false
		}
		out = append(append([]string(nil), list[:idx]...), append([]string{word}, list[idx:]...)...)
		outManual = manual
		if !containsSorted(manual, word) {
			outManual = append(append([]string(nil), manual...), word)
			sort.Strings(outManual)
		}
		return out, outManual, true
	}
	if idx >= len(list) || list[idx] != word {
		return list, manual, false
	}
	out = append(append([]string(nil), list[:idx]...), list[idx+1:]...)
	midx := sort.SearchStrings(manual, word)
	if midx < len(manual) && manual[midx] == word {
		outManual = append(append([]string(nil), manual[:midx]...), manual[midx+1:]...)
	} else {
		outManual = manual
	}
	return out, outManual, true
}

// validDLPWord trims and validates one word: non-empty, at most
// 128 runes, no control characters (a word list entry is a plain
// phrase, never a newline or a binary blob).
func validDLPWord(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > maxDLPWordRunes {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

func wordEntries(words, manual, template []string) []map[string]string {
	entries := make([]map[string]string, 0, len(words))
	for _, w := range words {
		src := "env"
		if containsSorted(manual, w) {
			src = "manual"
		} else if containsSorted(template, w) {
			src = "template"
		}
		entries = append(entries, map[string]string{"word": w, "source": src})
	}
	return entries
}

func containsSorted(sorted []string, w string) bool {
	i := sort.SearchStrings(sorted, w)
	return i < len(sorted) && sorted[i] == w
}

// liveWords returns the gateway's current effective lists (nil
// reader in tests means "nothing wired yet" — empty lists).
func (a *API) liveWords() (banned, custom []string) {
	if a.wordsReader == nil {
		return nil, nil
	}
	return a.wordsReader()
}

// dlpState returns the persisted managed lists (empty value when
// nothing was ever managed).
func (a *API) dlpState() *store.DLPWordLists {
	var dlp *store.DLPWordLists
	a.st.Read(func(st *store.State) { dlp = st.DLP })
	if dlp == nil {
		return &store.DLPWordLists{}
	}
	return dlp
}
