package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
	if stored.APIKeyEnc == "" || stored.APIKeyEnc == "lab-key" {
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
