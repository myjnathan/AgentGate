package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

// setupDLPAPI builds a seeded API whose "gateway" word lists are a
// fake the test can observe: the reader returns the current live
// lists, the applier replaces them. It mirrors the production boot
// lists (env defaults) so origins come out as "env".
func setupDLPAPI(t *testing.T) (*httptest.Server, *api.API, *store.Store, string, string, string, func() (banned, custom []string)) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secretbox.New("test-master-key-123456")
	au := auth.New(st)
	if err := seed.Demo(st, box, "http://127.0.0.1:9"); err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rest := api.New(st, au, box, al)
	liveBanned := append([]string(nil), seed.DemoBannedWords...)
	liveCustom := append([]string(nil), seed.DemoCustomWords...)
	rest.SetWordsReader(func() ([]string, []string) {
		return append([]string(nil), liveBanned...), append([]string(nil), liveCustom...)
	})
	rest.SetWordsApplier(func(banned, custom []string) {
		liveBanned = append([]string(nil), banned...)
		liveCustom = append([]string(nil), custom...)
	})
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	login := func(username, password string) string {
		s, d := callOnboard(t, srv, "POST", "/api/login", "", map[string]any{"username": username, "password": password})
		if s != 200 {
			t.Fatalf("login %s failed: %d %v", username, s, d)
		}
		return d["session"].(string)
	}
	admin := login("admin", "agentgate-admin")
	emp := login("lixiaofeng", "demo12345")
	live := func() ([]string, []string) { return liveBanned, liveCustom }
	return srv, rest, st, admin, emp, dir, live
}

func dlpWords(t *testing.T, srv *httptest.Server, sess string) (int, map[string]any) {
	t.Helper()
	return callOnboard(t, srv, "GET", "/api/dlp/words", sess, nil)
}

func wordsOf(t *testing.T, d map[string]any, list string) []map[string]any {
	t.Helper()
	raw, ok := d[list].([]any)
	if !ok {
		t.Fatalf("response lacks %q list: %v", list, d)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("word entry is not an object: %v", e)
		}
		out = append(out, m)
	}
	return out
}

func findWord(entries []map[string]any, word string) (map[string]any, bool) {
	for _, e := range entries {
		if e["word"] == word {
			return e, true
		}
	}
	return nil, false
}

func TestDLPWordsListOrigins(t *testing.T) {
	srv, _, _, admin, emp, _, _ := setupDLPAPI(t)

	s, d := dlpWords(t, srv, admin)
	if s != 200 {
		t.Fatalf("list failed: %d %v", s, d)
	}
	banned := wordsOf(t, d, "banned")
	if e, ok := findWord(banned, "test_sensitive"); !ok || e["source"] != "env" {
		t.Errorf("seed banned word must list as env origin: %v", banned)
	}
	custom := wordsOf(t, d, "custom")
	if e, ok := findWord(custom, "XH-200"); !ok || e["source"] != "env" {
		t.Errorf("seed custom word must list as env origin: %v", custom)
	}

	s, _ = dlpWords(t, srv, emp)
	if s != 403 {
		t.Errorf("employee must not read word lists: %d", s)
	}
	s, _ = dlpWords(t, srv, "")
	if s != 401 {
		t.Errorf("anonymous must not read word lists: %d", s)
	}
}

func TestDLPWordsAddRemoveRoundTrip(t *testing.T) {
	srv, _, _, admin, _, dir, live := setupDLPAPI(t)

	// Add a custom word: it shows up as manual, reaches the live
	// gateway lists, and is persisted across a store reopen.
	s, d := callOnboard(t, srv, "POST", "/api/dlp/words", admin, map[string]any{
		"list": "custom", "action": "add", "word": "冒烟测试词"})
	if s != 200 || d["changed"] != true || d["custom_count"] != float64(3) {
		t.Fatalf("add custom word failed: %d %v", s, d)
	}
	_, liveCustom := live()
	found := false
	for _, w := range liveCustom {
		if w == "冒烟测试词" {
			found = true
		}
	}
	if !found {
		t.Fatalf("live gateway lists lack the added word: %v", liveCustom)
	}
	s, d = dlpWords(t, srv, admin)
	if e, ok := findWord(wordsOf(t, d, "custom"), "冒烟测试词"); !ok || e["source"] != "manual" {
		t.Errorf("added word must list as manual origin: %v", d)
	}
	// Re-adding is an idempotent no-op, not an error.
	s, d = callOnboard(t, srv, "POST", "/api/dlp/words", admin, map[string]any{
		"list": "custom", "action": "add", "word": "冒烟测试词"})
	if s != 200 || d["changed"] != false {
		t.Fatalf("re-add must be idempotent: %d %v", s, d)
	}
	// Persistence: reopen the store and read the managed lists.
	st2, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var dlp *store.DLPWordLists
	st2.Read(func(s *store.State) { dlp = s.DLP })
	if dlp == nil || dlp.Custom == nil {
		t.Fatalf("managed lists must persist: %+v", dlp)
	}
	if _, ok := findWord(wordEntriesForTest(dlp.Custom), "冒烟测试词"); !ok {
		t.Errorf("persisted custom list lacks the word: %v", dlp.Custom)
	}

	// Remove an env-origin banned word: it disappears from the
	// effective lists (the managed copy is now the source of truth).
	s, d = callOnboard(t, srv, "POST", "/api/dlp/words", admin, map[string]any{
		"list": "banned", "action": "remove", "word": "test_sensitive"})
	if s != 200 || d["changed"] != true || d["banned_count"] != float64(0) {
		t.Fatalf("remove banned word failed: %d %v", s, d)
	}
	liveBanned, _ := live()
	if len(liveBanned) != 0 {
		t.Fatalf("live banned lists must be empty after removal: %v", liveBanned)
	}
	// Removing a word that is not there is a 400.
	s, _ = callOnboard(t, srv, "POST", "/api/dlp/words", admin, map[string]any{
		"list": "banned", "action": "remove", "word": "test_sensitive"})
	if s != 400 {
		t.Errorf("removing a missing word must be 400, got %d", s)
	}
	// Remove the manual word again: the manual origin is dropped too.
	s, _ = callOnboard(t, srv, "POST", "/api/dlp/words", admin, map[string]any{
		"list": "custom", "action": "remove", "word": "冒烟测试词"})
	if s != 200 {
		t.Fatalf("remove manual word failed: %d", s)
	}
	s, d = dlpWords(t, srv, admin)
	if _, ok := findWord(wordsOf(t, d, "custom"), "冒烟测试词"); ok {
		t.Errorf("removed word still listed: %v", d)
	}

	// The audit trail records the change without the word itself:
	// a word can be a project codename (sensitive original text).
	s, aud := callOnboard(t, srv, "GET", "/api/audit?limit=100", admin, nil)
	if s != 200 {
		t.Fatalf("audit read failed: %d", s)
	}
	raw, _ := json.Marshal(aud)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	events, _ := decoded["events"].([]any)
	seenChange := false
	for _, ev := range events {
		m, _ := ev.(map[string]any)
		if m == nil || m["action"] != "dlp_word_changed" {
			continue
		}
		seenChange = true
		if bytes.Contains(raw, []byte("冒烟测试词")) {
			// The word must not appear in the audit payload at all.
			t.Error("audit payload contains the word itself")
		}
	}
	if !seenChange {
		t.Error("no dlp_word_changed audit event found")
	}
}

func wordEntriesForTest(words []string) []map[string]any {
	out := make([]map[string]any, 0, len(words))
	for _, w := range words {
		out = append(out, map[string]any{"word": w})
	}
	return out
}

func TestDLPWordsValidation(t *testing.T) {
	srv, _, _, admin, emp, _, _ := setupDLPAPI(t)

	cases := []map[string]any{
		{"list": "custom", "action": "add", "word": ""},
		{"list": "custom", "action": "add", "word": "   "},
		{"list": "custom", "action": "add", "word": string(make([]byte, 200))},
		{"list": "custom", "action": "add", "word": "bad\nword"},
		{"list": "weird", "action": "add", "word": "x"},
		{"list": "custom", "action": "edit", "word": "x"},
	}
	for i, body := range cases {
		s, _ := callOnboard(t, srv, "POST", "/api/dlp/words", admin, body)
		if s != 400 {
			t.Errorf("case %d must be 400, got %d (%v)", i, s, body)
		}
	}
	// Employees may not change word lists at all.
	s, _ := callOnboard(t, srv, "POST", "/api/dlp/words", emp, map[string]any{
		"list": "custom", "action": "add", "word": "员工加词"})
	if s != 403 {
		t.Errorf("employee word change must be 403, got %d", s)
	}
}

func TestDLPWordsTemplateJoinsManagedList(t *testing.T) {
	srv, _, _, admin, _, _, _ := setupDLPAPI(t)

	// Manage the custom list first (add one manual word), then
	// apply the medical-device template: its words must join the
	// managed copy, or the next restart would drop them.
	s, _ := callOnboard(t, srv, "POST", "/api/dlp/words", admin, map[string]any{
		"list": "custom", "action": "add", "word": "手动词"})
	if s != 200 {
		t.Fatalf("manual add failed: %d", s)
	}
	s, d := callOnboard(t, srv, "POST", "/api/onboarding/apply-template", admin, map[string]any{
		"template_id": "medical-device"})
	if s != 200 {
		t.Fatalf("apply template failed: %d %v", s, d)
	}
	s, d = dlpWords(t, srv, admin)
	custom := wordsOf(t, d, "custom")
	for _, w := range []string{"手动词", "注册证号", "临床数据", "XH-200"} {
		e, ok := findWord(custom, w)
		if !ok {
			t.Errorf("custom list lacks %q after template: %v", w, custom)
			continue
		}
		want := "manual"
		if w != "手动词" {
			want = "template"
			if w == "XH-200" {
				// XH-200 is both a seed word and a template word;
				// template origin wins the display (it came from
				// the template's AppliedCustomWords record).
				want = "template"
			}
		}
		if e["source"] != want {
			t.Errorf("word %q origin %q, want %q", w, e["source"], want)
		}
	}
}

func TestChannelPresetsEndpoint(t *testing.T) {
	srv, _, _, admin, emp, _, _ := setupDLPAPI(t)

	s, d := callOnboard(t, srv, "GET", "/api/channels/presets", admin, nil)
	if s != 200 {
		t.Fatalf("presets failed: %d %v", s, d)
	}
	raw, ok := d["presets"].([]any)
	if !ok || len(raw) < 8 {
		t.Fatalf("expected >=8 presets, got %v", d)
	}
	byID := map[string]map[string]any{}
	for _, p := range raw {
		m, _ := p.(map[string]any)
		byID[m["id"].(string)] = m
	}
	ollama, ok := byID["ollama"]
	if !ok {
		t.Fatal("ollama preset missing")
	}
	if ollama["trust_type"] != "local_model" || ollama["max_data_level"] != "secret" {
		t.Errorf("ollama preset wrong: %v", ollama)
	}
	deepseek, ok := byID["deepseek"]
	if !ok || deepseek["base_url"] != "https://api.deepseek.com/v1" {
		t.Errorf("deepseek preset wrong: %v", deepseek)
	}
	if _, ok := byID["custom"]; !ok {
		t.Error("custom preset missing")
	}
	// Operator manages channels, so presets are visible to them;
	// employees never see them.
	ops := loginAs(t, srv.URL, "ops", "demo12345")
	s, _ = callOnboard(t, srv, "GET", "/api/channels/presets", ops, nil)
	if s != 200 {
		t.Errorf("operator must read presets: %d", s)
	}
	s, _ = callOnboard(t, srv, "GET", "/api/channels/presets", emp, nil)
	if s != 403 {
		t.Errorf("employee must not read presets: %d", s)
	}
}
