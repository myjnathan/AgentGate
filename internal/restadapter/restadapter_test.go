package restadapter_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/restadapter"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

type seenRequest struct {
	method string
	path   string
	query  string
	body   string
	auth   string
	ct     string
	header string
}

func setupAdapter(t *testing.T, handler http.HandlerFunc) (*restadapter.Adapter, string, *[]seenRequest) {
	t.Helper()
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, seenRequest{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery,
			body: string(b), auth: r.Header.Get("Authorization"),
			ct: r.Header.Get("Content-Type"), header: r.Header.Get("X-Demo")})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New("test-master-key-123456")
	if err != nil {
		t.Fatal(err)
	}
	return restadapter.New(st, box), srv.URL, &seen
}

func schema(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if err := restadapter.ValidateSchema(json.RawMessage(s)); err != nil {
		t.Fatalf("test schema invalid: %v", err)
	}
	return json.RawMessage(s)
}

func TestGETPathTemplateAndQuery(t *testing.T) {
	ad, base, seen := setupAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"balance": 12}`))
	})
	tool := &store.RESTTool{Name: "leave_balance", BaseURL: base, Method: "GET",
		Path: "/hr/{{employee_id}}/leave", InputSchema: schema(t,
			`{"type":"object","properties":{"employee_id":{"type":"string"},"year":{"type":"integer"}},"required":["employee_id"]}`)}
	text, isErr, err := ad.Call(context.Background(), tool,
		map[string]any{"employee_id": "li xiao", "year": float64(2026)})
	if err != nil || isErr {
		t.Fatalf("call: text=%q isErr=%v err=%v", text, isErr, err)
	}
	if text != `{"balance": 12}` {
		t.Fatalf("text = %q, want upstream body", text)
	}
	if len(*seen) != 1 {
		t.Fatalf("saw %d requests, want 1", len(*seen))
	}
	s := (*seen)[0]
	if s.method != "GET" {
		t.Fatalf("method = %q, want GET", s.method)
	}
	// Path param is URL-escaped; the remaining arg goes to the query.
	if s.path != "/hr/li%20xiao/leave" {
		t.Fatalf("path = %q, want /hr/li%%20xiao/leave", s.path)
	}
	if s.query != "year=2026" {
		t.Fatalf("query = %q, want year=2026", s.query)
	}
}

func TestPOSTJSONBodyAndCredential(t *testing.T) {
	ad, base, seen := setupAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	box, _ := secretbox.New("test-master-key-123456")
	enc, err := box.Encrypt("demo-credential")
	if err != nil {
		t.Fatal(err)
	}
	tool := &store.RESTTool{Name: "submit", BaseURL: base, Method: "POST",
		Path: "/leave", Headers: map[string]string{"X-Demo": "yes"},
		AuthTokenEnc: enc, InputSchema: schema(t,
			`{"type":"object","properties":{"employee_id":{"type":"string"},"days":{"type":"integer"}},"required":["employee_id","days"]}`)}
	text, isErr, err := ad.Call(context.Background(), tool,
		map[string]any{"employee_id": "lixiaofeng", "days": float64(2)})
	if err != nil || isErr || text != `{"ok":true}` {
		t.Fatalf("call: text=%q isErr=%v err=%v", text, isErr, err)
	}
	s := (*seen)[0]
	if s.method != "POST" || s.ct != "application/json" {
		t.Fatalf("method/ct = %q/%q, want POST/application/json", s.method, s.ct)
	}
	if s.auth != "Bearer demo-credential" {
		t.Fatalf("auth header = %q, want Bearer demo-credential", s.auth)
	}
	if s.header != "yes" {
		t.Fatalf("static header = %q, want yes", s.header)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(s.body), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["employee_id"] != "lixiaofeng" || body["days"] != float64(2) {
		t.Fatalf("body = %v, want employee_id + days", body)
	}
}

func TestSchemaValidation(t *testing.T) {
	ad, base, _ := setupAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	tool := &store.RESTTool{Name: "leave_balance", BaseURL: base, Method: "GET",
		Path: "/leave", InputSchema: schema(t,
			`{"type":"object","properties":{"employee_id":{"type":"string"},"kind":{"type":"string","enum":["annual","sick"]}},"required":["employee_id"]}`)}
	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing required", map[string]any{}},
		{"wrong type", map[string]any{"employee_id": float64(3)}},
		{"unknown argument", map[string]any{"employee_id": "li", "surprise": true}},
		{"enum violation", map[string]any{"employee_id": "li", "kind": "other"}},
	}
	for _, c := range cases {
		if _, _, err := ad.Call(context.Background(), tool, c.args); err == nil {
			t.Fatalf("%s: expected validation error, got none", c.name)
		}
	}
	// Valid call reaches the upstream.
	if _, _, err := ad.Call(context.Background(), tool,
		map[string]any{"employee_id": "li", "kind": "sick"}); err != nil {
		t.Fatalf("valid call: unexpected error %v", err)
	}
}

func TestUpstreamErrorDoesNotLeakBody(t *testing.T) {
	ad, base, _ := setupAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"internal": "db password hunter2"}`))
	})
	tool := &store.RESTTool{Name: "leave_balance", BaseURL: base, Method: "GET",
		Path: "/leave", InputSchema: schema(t, `{"type":"object","properties":{}}`)}
	text, isErr, err := ad.Call(context.Background(), tool, map[string]any{})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !isErr {
		t.Fatalf("5xx must be an error result")
	}
	if strings.Contains(text, "hunter2") || !strings.Contains(text, "500") {
		t.Fatalf("error text must name the status, not the body: %q", text)
	}
}

func TestValidateSchemaRejects(t *testing.T) {
	bad := []string{
		`{"type":"array"}`,
		`{"type":"object","properties":{"x":{"type":"date"}}}`,
		`{"type":"object","properties":{"x":{"type":"string"}},"required":["y"]}`,
		`not json`,
		``,
	}
	for _, s := range bad {
		if err := restadapter.ValidateSchema(json.RawMessage(s)); err == nil {
			t.Fatalf("schema %q: expected rejection", s)
		}
	}
}

func TestToolNameAndMethod(t *testing.T) {
	for _, n := range []string{"leave_balance", "hr2"} {
		if !restadapter.ValidToolName(n) {
			t.Fatalf("name %q should be valid", n)
		}
	}
	for _, n := range []string{"Leave", "leave-balance", "hr.tool", "", "1st"} {
		if restadapter.ValidToolName(n) {
			t.Fatalf("name %q should be invalid", n)
		}
	}
	for _, m := range []string{"GET", "post", "DELETE"} {
		if !restadapter.ValidMethod(m) {
			t.Fatalf("method %q should be valid", m)
		}
	}
	if restadapter.ValidMethod("HEAD") || restadapter.ValidMethod("OPTIONS") {
		t.Fatal("HEAD/OPTIONS must not be usable methods")
	}
}
