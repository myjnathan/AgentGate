// Package restadapter turns a registered REST endpoint into an MCP
// tool (backlog F7, borrowing IBM ContextForge's REST-tool model: one
// JSON registration per endpoint — name, method, URL, input schema —
// with the hosted credential injected at call time). The adapter only
// moves bytes: argument validation, tier authorization, data-level
// gating and DLP live in the calling layer (internal/mcp), so a REST
// tool can never bypass platform policy. OpenAPI import is
// intentionally out of scope for this round — the input schema is
// supplied by the admin at registration; ContextForge itself only
// generates a schema from a spec one endpoint at a time.
//
// Request mapping: {{param}} placeholders in the path are substituted
// (URL-escaped) from the arguments; the remaining arguments go to the
// query string for GET/DELETE or to a JSON body for POST/PUT/PATCH.
// Upstream error bodies are never returned — they may carry
// upstream-internal detail — only the status code is reported.
package restadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// ErrValidation marks an argument-validation failure (missing or
// mistyped argument, unknown argument, missing path parameter).
// Callers map it to JSON-RPC -32602 (invalid params); anything else
// from Call is an upstream/transport failure.
var ErrValidation = errors.New("invalid arguments")

// MaxResponseBytes caps a single upstream response so one large
// answer cannot flood the model's context (or the server's memory).
const MaxResponseBytes = 1 << 20

var (
	validToolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	pathTemplate  = regexp.MustCompile(`\{\{([a-zA-Z0-9_]+)\}\}`)
)

// ValidToolName reports whether name is a usable REST tool name
// (lowercase snake_case; exposed as "rest.<name>").
func ValidToolName(name string) bool { return validToolName.MatchString(name) }

// ValidMethod reports whether m is an HTTP method a REST tool may use.
func ValidMethod(m string) bool {
	switch strings.ToUpper(m) {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

var knownPropTypes = map[string]bool{
	"string": true, "integer": true, "number": true,
	"boolean": true, "array": true, "object": true,
}

// ValidateSchema checks an admin-supplied input schema: it must be a
// JSON object schema whose properties have known types and whose
// required list only names declared properties. Only the subset of
// JSON Schema the adapter enforces is accepted, so a schema that
// looks valid but cannot be validated is rejected at registration
// time rather than at first call.
func ValidateSchema(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("input_schema is required")
	}
	var sch struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
			Enum []any  `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &sch); err != nil {
		return fmt.Errorf("input_schema is not valid JSON: %w", err)
	}
	if sch.Type != "" && sch.Type != "object" {
		return fmt.Errorf("input_schema type must be object")
	}
	for name, p := range sch.Properties {
		if p.Type != "" && !knownPropTypes[p.Type] {
			return fmt.Errorf("input_schema property %q has unknown type %q", name, p.Type)
		}
		if len(p.Enum) == 0 && p.Enum != nil {
			return fmt.Errorf("input_schema property %q has an empty enum", name)
		}
	}
	for _, req := range sch.Required {
		if _, ok := sch.Properties[req]; !ok {
			return fmt.Errorf("input_schema required %q is not declared in properties", req)
		}
	}
	return nil
}

// Adapter holds the store and credential box; it is stateless apart
// from the HTTP client.
type Adapter struct {
	st   *store.Store
	box  *secretbox.Box
	http *http.Client
}

func New(st *store.Store, box *secretbox.Box) *Adapter {
	return &Adapter{st: st, box: box, http: &http.Client{Timeout: 10 * time.Second}}
}

// Tool returns a copy of the registered REST tool with this name.
func (a *Adapter) Tool(name string) (*store.RESTTool, bool) {
	var found *store.RESTTool
	a.st.Read(func(st *store.State) {
		for _, t := range st.RESTTools {
			if t.Name == name {
				cp := *t
				found = &cp
			}
		}
	})
	return found, found != nil
}

// Enabled returns copies of all enabled REST tools.
func (a *Adapter) Enabled() []*store.RESTTool {
	var out []*store.RESTTool
	a.st.Read(func(st *store.State) {
		for _, t := range st.RESTTools {
			if t.Enabled {
				cp := *t
				out = append(out, &cp)
			}
		}
	})
	return out
}

// ValidateArgs checks arguments against the tool's input schema:
// required fields must be present, values must match their declared
// type and enum, and undeclared arguments are rejected (the schema is
// the contract — an undeclared argument would otherwise be silently
// forwarded to the upstream). The error names the offending field
// only, never its value.
func ValidateArgs(tool *store.RESTTool, args map[string]any) error {
	var sch struct {
		Properties map[string]struct {
			Type string `json:"type"`
			Enum []any  `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.InputSchema, &sch); err != nil {
		return fmt.Errorf("tool %q has an unreadable input schema", tool.Name)
	}
	for _, req := range sch.Required {
		if _, ok := args[req]; !ok {
			return fmt.Errorf("missing required argument %q", req)
		}
	}
	for name, v := range args {
		prop, ok := sch.Properties[name]
		if !ok {
			return fmt.Errorf("unknown argument %q", name)
		}
		if err := checkType(name, prop.Type, v); err != nil {
			return err
		}
		if len(prop.Enum) > 0 {
			match := false
			for _, e := range prop.Enum {
				if fmt.Sprintf("%v", e) == fmt.Sprintf("%v", v) {
					match = true
					break
				}
			}
			if !match {
				return fmt.Errorf("argument %q is not one of the allowed values", name)
			}
		}
	}
	return nil
}

func checkType(name, typ string, v any) error {
	if typ == "" {
		return nil
	}
	switch typ {
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("argument %q must be a string", name)
		}
	case "integer":
		f, ok := v.(float64)
		if !ok || f != float64(int64(f)) {
			return fmt.Errorf("argument %q must be an integer", name)
		}
	case "number":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("argument %q must be a number", name)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("argument %q must be a boolean", name)
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("argument %q must be an array", name)
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("argument %q must be an object", name)
		}
	}
	return nil
}

// Call validates the arguments, builds the HTTP request, injects the
// hosted credential (decrypted only for this request) and returns the
// upstream response body as text. isError is true when the upstream
// answered with a non-2xx status; the upstream body is not returned
// in that case. Transport failures and oversized responses are Go
// errors.
func (a *Adapter) Call(ctx context.Context, tool *store.RESTTool, args map[string]any) (string, bool, error) {
	if args == nil {
		args = map[string]any{}
	}
	if err := ValidateArgs(tool, args); err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	remaining := map[string]any{}
	for k, v := range args {
		remaining[k] = v
	}
	path := tool.Path
	for _, m := range pathTemplate.FindAllStringSubmatch(tool.Path, -1) {
		name := m[1]
		v, ok := remaining[name]
		if !ok {
			return "", false, fmt.Errorf("%w: missing path argument %q", ErrValidation, name)
		}
		s, ok := scalarString(v)
		if !ok {
			return "", false, fmt.Errorf("%w: path argument %q must be a scalar", ErrValidation, name)
		}
		path = strings.ReplaceAll(path, m[0], url.PathEscape(s))
		delete(remaining, name)
	}
	u, err := url.Parse(strings.TrimSuffix(tool.BaseURL, "/") + path)
	if err != nil {
		return "", false, fmt.Errorf("invalid tool URL: %w", err)
	}
	method := strings.ToUpper(tool.Method)
	var body io.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		q := u.Query()
		for k, v := range remaining {
			s, ok := scalarString(v)
			if !ok {
				b, _ := json.Marshal(v)
				s = string(b)
			}
			q.Set(k, s)
		}
		u.RawQuery = q.Encode()
	} else if len(remaining) > 0 {
		b, err := json.Marshal(remaining)
		if err != nil {
			return "", false, fmt.Errorf("marshal request body: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return "", false, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range tool.Headers {
		req.Header.Set(k, v)
	}
	if tool.AuthTokenEnc != "" {
		tok, err := a.box.Decrypt(tool.AuthTokenEnc)
		if err != nil {
			return "", false, fmt.Errorf("decrypt tool credential: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode), true, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > MaxResponseBytes {
		return "", false, fmt.Errorf("upstream response exceeds %d bytes", MaxResponseBytes)
	}
	return string(b), false, nil
}

// scalarString renders a JSON scalar for use in a path segment or a
// query value; non-scalars report false.
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	case json.Number:
		return t.String(), true
	}
	return "", false
}
