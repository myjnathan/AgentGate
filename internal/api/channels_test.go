package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

func TestChannelsTrustType(t *testing.T) {
	srv, st, adminSess, empSess := setupTokensAPI(t)

	call := func(method, path, sess string, body any) (int, map[string]any) {
		t.Helper()
		var buf *bytes.Buffer
		if body != nil {
			b, _ := json.Marshal(body)
			buf = bytes.NewBuffer(b)
		} else {
			buf = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, buf)
		req.Header.Set("Content-Type", "application/json")
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Seeded demo channels carry their F17 trust types.
	code, listed := call("GET", "/api/channels", adminSess, nil)
	if code != 200 {
		t.Fatalf("list channels: got %d, want 200", code)
	}
	byName := map[string]string{}
	if chans, ok := listed["channels"].([]any); ok {
		for _, c := range chans {
			m, _ := c.(map[string]any)
			name, _ := m["name"].(string)
			tt, _ := m["trust_type"].(string)
			byName[name] = tt
		}
	}
	for name, want := range map[string]string{
		"cloud-contract (mock)":              "signed_cloud",
		"cloud-contract-desensitized (mock)": "signed_cloud",
		"private-onprem (mock)":              "dedicated_cloud",
		"local-model (mock)":                 "local_model",
	} {
		if got := byName[name]; got != want {
			t.Fatalf("channel %q trust_type = %q, want %q", name, got, want)
		}
	}

	// Employee may not create a channel.
	if code, _ := call("POST", "/api/channels", empSess, map[string]any{
		"name": "x", "base_url": "https://llm.example.invalid/v1",
		"models": []string{"mock-eco"}}); code != 403 {
		t.Fatalf("employee create channel: got %d, want 403", code)
	}
	// Unknown trust type is rejected, not silently defaulted.
	if code, _ := call("POST", "/api/channels", adminSess, map[string]any{
		"name": "x", "base_url": "https://llm.example.invalid/v1",
		"models": []string{"mock-eco"}, "trust_type": "onprem"}); code != 400 {
		t.Fatalf("bad trust_type: got %d, want 400", code)
	}

	// Explicit local-model channel.
	code, created := call("POST", "/api/channels", adminSess, map[string]any{
		"name": "lab-local", "base_url": "https://llm.example.invalid/v1",
		"api_key": "lab-key", "models": []string{"mock-pro"},
		"max_data_level": "secret", "trust_type": "local_model"})
	if code != 201 {
		t.Fatalf("create local-model channel: got %d (%v), want 201", code, created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("create channel returned no id")
	}
	var stored *store.Channel
	st.Read(func(s *store.State) { stored = s.Channels[id] })
	if stored == nil || stored.TrustType != store.TrustLocalModel {
		t.Fatalf("stored trust type = %v, want local_model", stored)
	}
	if len(stored.Keys) != 1 || stored.Keys[0].Enc == "" || stored.Keys[0].Enc == "lab-key" {
		t.Fatal("channel key must be stored encrypted")
	}

	// Empty trust_type defaults to signed cloud.
	code, created = call("POST", "/api/channels", adminSess, map[string]any{
		"name": "lab-cloud", "base_url": "https://llm.example.invalid/v1",
		"models": []string{"mock-eco"}, "max_data_level": "internal"})
	if code != 201 {
		t.Fatalf("create channel without trust_type: got %d (%v), want 201", code, created)
	}
	id, _ = created["id"].(string)
	st.Read(func(s *store.State) { stored = s.Channels[id] })
	if stored == nil || stored.TrustType != store.TrustSignedCloud {
		t.Fatalf("default trust type = %v, want signed_cloud", stored)
	}

	// The API key is never exposed in the list response.
	code, listed = call("GET", "/api/channels", adminSess, nil)
	if code != 200 {
		t.Fatalf("list channels: got %d, want 200", code)
	}
	raw, _ := json.Marshal(listed)
	if strings.Contains(string(raw), "lab-key") {
		t.Fatal("list response leaks the channel API key")
	}
}

func TestChannelsMultiKey(t *testing.T) {
	srv, st, adminSess, empSess := setupTokensAPI(t)

	call := func(method, path, sess string, body any) (int, map[string]any) {
		t.Helper()
		var buf *bytes.Buffer
		if body != nil {
			b, _ := json.Marshal(body)
			buf = bytes.NewBuffer(b)
		} else {
			buf = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, buf)
		req.Header.Set("Content-Type", "application/json")
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	keysOf := func(id string) []any {
		t.Helper()
		_, listed := call("GET", "/api/channels", adminSess, nil)
		for _, c := range listed["channels"].([]any) {
			m, _ := c.(map[string]any)
			if m["id"] == id {
				ks, _ := m["keys"].([]any)
				return ks
			}
		}
		t.Fatalf("channel %s not listed", id)
		return nil
	}

	// Create with one primary + two extra keys.
	code, created := call("POST", "/api/channels", adminSess, map[string]any{
		"name": "multi (test)", "base_url": "https://llm.example.invalid/v1",
		"api_key": "key-one", "api_keys": []string{"key-two", "key-three"},
		"models": []string{"mock-eco"}, "max_data_level": "internal"})
	if code != 201 {
		t.Fatalf("create multi-key channel: got %d (%v), want 201", code, created)
	}
	id, _ := created["id"].(string)
	if created["key_count"] != float64(3) {
		t.Fatalf("key_count = %v, want 3", created["key_count"])
	}
	ks := keysOf(id)
	if len(ks) != 3 {
		t.Fatalf("listed keys = %d, want 3", len(ks))
	}
	for i, want := range []string{"****-one", "****-two", "****hree"} {
		m, _ := ks[i].(map[string]any)
		if m["index"] != float64(i) || m["masked"] != want || m["disabled"] != false {
			t.Fatalf("key %d = %v, want index %d masked %q enabled", i, m, i, want)
		}
	}
	var stored *store.Channel
	st.Read(func(s *store.State) { stored = s.Channels[id] })
	for i, plain := range []string{"key-one", "key-two", "key-three"} {
		if stored.Keys[i].Enc == "" || stored.Keys[i].Enc == plain {
			t.Fatalf("key %d not stored encrypted", i)
		}
	}
	// Duplicate keys are rejected at creation.
	if code, _ := call("POST", "/api/channels", adminSess, map[string]any{
		"name": "dup (test)", "base_url": "https://llm.example.invalid/v1",
		"api_key": "same-key", "api_keys": []string{"same-key"},
		"models": []string{"mock-eco"}}); code != 400 {
		t.Fatalf("duplicate key at create: got %d, want 400", code)
	}

	// Add a key, then disable / re-enable it, then delete it.
	code, added := call("POST", "/api/channels/"+id+"/keys", adminSess, map[string]any{"api_key": "key-four"})
	if code != 201 || added["index"] != float64(3) || added["key_masked"] != "****four" {
		t.Fatalf("add key: got %d (%v), want 201 index 3 masked ****four", code, added)
	}
	if code, _ := call("POST", "/api/channels/"+id+"/keys", adminSess, map[string]any{"api_key": "key-two"}); code != 400 {
		t.Fatalf("duplicate key at add: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/channels/"+id+"/keys/3/disable", adminSess, map[string]any{"reason": "leak suspected"}); code != 200 {
		t.Fatalf("disable key: got %d, want 200", code)
	}
	ks = keysOf(id)
	m3, _ := ks[3].(map[string]any)
	if m3["disabled"] != true || m3["disabled_reason"] != "leak suspected" {
		t.Fatalf("key 3 after disable = %v, want disabled with reason", m3)
	}
	if code, _ := call("POST", "/api/channels/"+id+"/keys/3/enable", adminSess, nil); code != 200 {
		t.Fatalf("enable key: got %d, want 200", code)
	}
	ks = keysOf(id)
	m3, _ = ks[3].(map[string]any)
	if m3["disabled"] != false {
		t.Fatalf("key 3 after enable = %v, want enabled", m3)
	}
	if code, _ := call("DELETE", "/api/channels/"+id+"/keys/3", adminSess, nil); code != 200 {
		t.Fatalf("delete key: got %d, want 200", code)
	}
	if len(keysOf(id)) != 3 {
		t.Fatal("key count after delete != 3")
	}

	// The last key cannot be deleted; bad indexes 404; employees may
	// not touch keys at all.
	if code, _ := call("DELETE", "/api/channels/"+id+"/keys/9", adminSess, nil); code != 404 {
		t.Fatalf("delete missing key: got %d, want 404", code)
	}
	if code, _ := call("POST", "/api/channels/"+id+"/keys/0/disable", empSess, map[string]any{}); code != 403 {
		t.Fatalf("employee disable key: got %d, want 403", code)
	}
	if code, _ := call("POST", "/api/channels/"+id+"/keys", empSess, map[string]any{"api_key": "x"}); code != 403 {
		t.Fatalf("employee add key: got %d, want 403", code)
	}
	singleID := ""
	if code, created := call("POST", "/api/channels", adminSess, map[string]any{
		"name": "single (test)", "base_url": "https://llm.example.invalid/v1",
		"api_key": "only-key", "models": []string{"mock-eco"}}); code != 201 {
		t.Fatalf("create single-key channel: got %d, want 201", code)
	} else {
		singleID, _ = created["id"].(string)
	}
	if code, _ := call("DELETE", "/api/channels/"+singleID+"/keys/0", adminSess, nil); code != 400 {
		t.Fatalf("delete last key: got %d, want 400", code)
	}

	// No plaintext key anywhere in any response seen this round.
	code, listed := call("GET", "/api/channels", adminSess, nil)
	if code != 200 {
		t.Fatalf("list channels: got %d, want 200", code)
	}
	raw, _ := json.Marshal(listed)
	for _, plain := range []string{"key-one", "key-two", "key-three", "key-four", "only-key"} {
		if strings.Contains(string(raw), plain) {
			t.Fatalf("list response leaks plaintext key %q", plain)
		}
	}
}

// TestChannelKeysSurviveRestart is the API-level pin for the 2026-11-25
// fix: keys added through the API must still be there — decryptable —
// after the store is reopened from disk (the F27 drill found channels
// came back with zero keys after a volume restore, and every model call
// failed with 502 channel_no_enabled_key). It also pins that a key
// disabled by hand reloads enabled (F12: disabled state is runtime-only)
// and that no ciphertext ever leaves through the API.
func TestChannelKeysSurviveRestart(t *testing.T) {
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
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	login := func(username, password string) string {
		t.Helper()
		body := strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`)
		resp, err := http.Post(srv.URL+"/api/login", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d struct {
			Session string `json:"session"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&d)
		if resp.StatusCode != 200 || d.Session == "" {
			t.Fatalf("login %s failed: %d", username, resp.StatusCode)
		}
		return d.Session
	}
	admin := login("admin", "agentgate-admin")
	call := func(method, path, sess string, body any) (int, map[string]any) {
		t.Helper()
		var buf *bytes.Buffer
		if body != nil {
			b, _ := json.Marshal(body)
			buf = bytes.NewBuffer(b)
		} else {
			buf = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, buf)
		req.Header.Set("Content-Type", "application/json")
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	code, created := call("POST", "/api/channels", admin, map[string]any{
		"name": "persist (test)", "base_url": "https://llm.example.invalid/v1",
		"api_key": "sk-persist-drill-1", "models": []string{"mock-eco"}, "max_data_level": "internal"})
	if code != 201 {
		t.Fatalf("create channel: got %d (%v), want 201", code, created)
	}
	id, _ := created["id"].(string)
	if code, _ := call("POST", "/api/channels/"+id+"/keys", admin, map[string]any{"api_key": "sk-persist-drill-2"}); code != 201 {
		t.Fatalf("add key: got %d, want 201", code)
	}
	// Disable the second key by hand, then reopen: the ciphertext
	// must survive, the disabled flag must not.
	if code, _ := call("POST", "/api/channels/"+id+"/keys/1/disable", admin, map[string]any{"reason": "drill"}); code != 200 {
		t.Fatalf("disable key: got %d, want 200", code)
	}
	// Neither plaintext nor ciphertext may leave through the API.
	code, listed := call("GET", "/api/channels", admin, nil)
	if code != 200 {
		t.Fatalf("list channels: got %d, want 200", code)
	}
	raw, _ := json.Marshal(listed)
	for _, leak := range []string{"sk-persist-drill-1", "sk-persist-drill-2", `"enc"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("list response leaks %q", leak)
		}
	}
	// Reopen the store from disk, as a restart or a volume restore
	// would: both keys come back decryptable, both enabled.
	st2, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ch *store.Channel
	st2.Read(func(s *store.State) { ch = s.Channels[id] })
	if ch == nil || len(ch.Keys) != 2 {
		t.Fatalf("reloaded channel has %d keys, want 2", len(ch.Keys))
	}
	for i, want := range []string{"sk-persist-drill-1", "sk-persist-drill-2"} {
		plain, err := box.Decrypt(ch.Keys[i].Enc)
		if err != nil || plain != want {
			t.Fatalf("key %d decrypts to %q (%v), want %q", i, plain, err, want)
		}
		if ch.Keys[i].Disabled {
			t.Fatalf("key %d must reload enabled (disabled state is runtime-only)", i)
		}
	}
}
