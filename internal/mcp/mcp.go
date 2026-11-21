// Package mcp exposes AgentGate's capabilities as a remote MCP server
// (JSON-RPC 2.0 over HTTP, streamable-HTTP style) so any MCP-capable
// desktop agent can consume enterprise tools without a bespoke plugin.
// Tool-level authorization is enforced server-side per token tier, and
// every tool call is audited. Tool results pass a DLP scan before they
// are returned, because tool output is the second entry point for
// sensitive data into a model's context.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/kb"
	"github.com/myjnathan/agentgate/internal/restadapter"
	"github.com/myjnathan/agentgate/internal/store"
)

type Server struct {
	st    *store.Store
	auth  *auth.Service
	audit *audit.Log
	det   *guard.Detector
	fed   *federation.Registry
	rest  *restadapter.Adapter
	// ToolMinTier: minimum tier required per tool (default BYOA).
	ToolMinTier map[string]store.Tier
}

func New(st *store.Store, au *auth.Service, al *audit.Log, det *guard.Detector, fed *federation.Registry, rest *restadapter.Adapter) *Server {
	return &Server{st: st, auth: au, audit: al, det: det, fed: fed, rest: rest, ToolMinTier: map[string]store.Tier{
		"kb_get": store.TierManaged, // full-text reads of confidential docs: managed only
	}}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

var tools = []toolDef{
	{
		Name:        "kb_search",
		Description: "Search the enterprise knowledge base by keywords. Returns cited excerpts. Call repeatedly with refined keywords, then use kb_get to read the original document.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "space-separated keywords"},
		}, "required": []string{"query"}},
	},
	{
		Name:        "kb_get",
		Description: "Read the full text of a knowledge-base document by id (managed-tier agents only for confidential documents).",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"doc_id": map[string]any{"type": "string"},
		}, "required": []string{"doc_id"}},
	},
	{
		Name:        "whoami",
		Description: "Return the identity, department and agent tier bound to the current token.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h := r.Header.Get("Authorization")
	tok, user, ok := s.auth.ResolveToken(strings.TrimPrefix(h, "Bearer "))
	if !ok {
		writeRPCError(w, nil, -32001, "unauthorized")
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	switch req.Method {
	case "initialize":
		writeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agentgate", "version": "0.1.0"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		writeRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": s.listTools(r.Context(), tok)})
	case "tools/call":
		s.callTool(w, r.Context(), req, tok, user)
	default:
		writeRPCError(w, req.ID, -32601, "method not found")
	}
}

// listTools merges the built-in tools with federated upstream tools
// (backlog F6) and REST-adapted tools (backlog F7, exposed as
// "rest.<name>"). REST tools above the caller's tier are hidden, the
// same as federated ones.
func (s *Server) listTools(ctx context.Context, tok *store.APIToken) []toolDef {
	out := append([]toolDef{}, tools...)
	if s.fed != nil {
		for _, up := range s.fed.Enabled() {
			if tierRank(tok.Tier) < tierRank(up.MinTier) {
				continue
			}
			uts, err := s.fed.ListTools(ctx, up)
			if err != nil {
				continue
			}
			for _, t := range uts {
				out = append(out, toolDef{
					Name:        up.Name + "." + t.Name,
					Description: "[" + up.Name + "] " + t.Description,
					InputSchema: t.InputSchema,
				})
			}
		}
	}
	if s.rest != nil {
		for _, rt := range s.rest.Enabled() {
			if tierRank(tok.Tier) < tierRank(rt.MinTier) {
				continue
			}
			out = append(out, toolDef{
				Name:        "rest." + rt.Name,
				Description: "[REST] " + rt.Description,
				InputSchema: rt.InputSchema,
			})
		}
	}
	return out
}

// callFederatedTool forwards a "<namespace>.<tool>" call to its
// upstream after the platform gates: tier authorization, then a DLP
// scan of the arguments — banned content is blocked outright, and
// arguments classified above the upstream's MaxDataLevel are never
// sent (data classification outranks availability). The result gets
// the same DLP scan and BYOA redaction as built-in tools.
func (s *Server) callFederatedTool(w http.ResponseWriter, ctx context.Context, req rpcRequest, tok *store.APIToken, user *store.User, namespace, tool string, args map[string]any) {
	up, ok := s.fed.Upstream(namespace)
	if !ok || !up.Enabled {
		writeRPCError(w, req.ID, -32602, "unknown tool")
		return
	}
	fullName := namespace + "." + tool
	deny := func(reason string) {
		s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "tool_call_denied", Decision: "block",
			Detail: map[string]string{"tool": fullName, "upstream": namespace, "reason": reason}})
	}
	if tierRank(tok.Tier) < tierRank(up.MinTier) {
		deny("tier_insufficient")
		writeRPCError(w, req.ID, -32003, "tool not permitted for this agent tier")
		return
	}
	argJSON, _ := json.Marshal(args)
	findings := s.det.Scan(string(argJSON))
	for _, f := range findings {
		if f.Category == guard.CatBanned {
			deny("banned_content")
			writeRPCError(w, req.ID, -32004, "request blocked by data policy")
			return
		}
	}
	if lvl := guard.Classify(findings); lvl > up.MaxDataLevel {
		deny("data_level_exceeds_upstream")
		writeRPCError(w, req.ID, -32004, "request blocked by data policy")
		return
	}

	text, isErr, err := s.fed.CallTool(ctx, up, tool, args)
	if err != nil {
		s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "tool_call", Action: fullName, Decision: "error",
			Detail: map[string]string{"tool": fullName, "upstream": namespace}})
		writeRPCError(w, req.ID, -32002, "upstream tool call failed")
		return
	}

	// DLP scan of tool output before it enters the model's context.
	findings = s.det.Scan(text)
	level := guard.Classify(findings)
	if tok.Tier == store.TierBYOA && level >= store.LevelConfidential {
		red, _ := guard.Redact(text, findings)
		text = red
	}

	s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "tool_call", Action: fullName, DataLevel: level.String(), Decision: "allow",
		Detail: map[string]string{"tool": fullName, "upstream": namespace}})
	writeRPCResult(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	})
}

// callRESTTool handles a "rest.<name>" call: the same platform gates
// as a federated call (tier authorization, then a DLP scan of the
// arguments — banned content is blocked outright, and arguments
// classified above the tool's MaxDataLevel are never sent), then the
// adapter translates the call into one HTTP request. Argument
// validation failures are -32602 (invalid params); transport failures
// are -32002. The result gets the same DLP scan and BYOA redaction as
// built-in tools.
func (s *Server) callRESTTool(w http.ResponseWriter, ctx context.Context, req rpcRequest, tok *store.APIToken, user *store.User, name string, args map[string]any) {
	rt, ok := s.rest.Tool(name)
	if !ok || !rt.Enabled {
		writeRPCError(w, req.ID, -32602, "unknown tool")
		return
	}
	fullName := "rest." + name
	deny := func(reason string) {
		s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "tool_call_denied", Decision: "block",
			Detail: map[string]string{"tool": fullName, "rest_tool": name, "reason": reason}})
	}
	if tierRank(tok.Tier) < tierRank(rt.MinTier) {
		deny("tier_insufficient")
		writeRPCError(w, req.ID, -32003, "tool not permitted for this agent tier")
		return
	}
	argJSON, _ := json.Marshal(args)
	findings := s.det.Scan(string(argJSON))
	for _, f := range findings {
		if f.Category == guard.CatBanned {
			deny("banned_content")
			writeRPCError(w, req.ID, -32004, "request blocked by data policy")
			return
		}
	}
	if lvl := guard.Classify(findings); lvl > rt.MaxDataLevel {
		deny("data_level_exceeds_tool")
		writeRPCError(w, req.ID, -32004, "request blocked by data policy")
		return
	}

	text, isErr, err := s.rest.Call(ctx, rt, args)
	if err != nil {
		// Validation errors (missing/wrong-typed argument) are invalid
		// params; anything else is an upstream failure. The message
		// names the field, never its value.
		code := -32002
		msg := "upstream tool call failed"
		if errors.Is(err, restadapter.ErrValidation) {
			code, msg = -32602, err.Error()
		}
		s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "tool_call", Action: fullName, Decision: "error",
			Detail: map[string]string{"tool": fullName, "rest_tool": name}})
		writeRPCError(w, req.ID, code, msg)
		return
	}

	// DLP scan of tool output before it enters the model's context.
	findings = s.det.Scan(text)
	level := guard.Classify(findings)
	if tok.Tier == store.TierBYOA && level >= store.LevelConfidential {
		red, _ := guard.Redact(text, findings)
		text = red
	}

	s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "tool_call", Action: fullName, DataLevel: level.String(), Decision: "allow",
		Detail: map[string]string{"tool": fullName, "rest_tool": name}})
	writeRPCResult(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	})
}

func (s *Server) callTool(w http.ResponseWriter, ctx context.Context, req rpcRequest, tok *store.APIToken, user *store.User) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &params)

	// REST-adapted tools ("rest.<name>", F7) take precedence over the
	// federated namespace dispatch below.
	if strings.HasPrefix(params.Name, "rest.") {
		if s.rest == nil {
			writeRPCError(w, req.ID, -32602, "unknown tool")
			return
		}
		s.callRESTTool(w, ctx, req, tok, user, params.Name[len("rest."):], params.Arguments)
		return
	}

	// Namespaced tools ("<upstream>.<tool>") are federated (F6).
	if i := strings.Index(params.Name, "."); i > 0 {
		if s.fed == nil {
			writeRPCError(w, req.ID, -32602, "unknown tool")
			return
		}
		s.callFederatedTool(w, ctx, req, tok, user, params.Name[:i], params.Name[i+1:], params.Arguments)
		return
	}

	// Tool-level authorization.
	if min, ok := s.ToolMinTier[params.Name]; ok && tierRank(tok.Tier) < tierRank(min) {
		s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "tool_call_denied", Decision: "block",
			Detail: map[string]string{"tool": params.Name, "reason": "tier_insufficient"}})
		writeRPCError(w, req.ID, -32003, "tool not permitted for this agent tier")
		return
	}

	var text string
	var isErr bool
	switch params.Name {
	case "whoami":
		b, _ := json.Marshal(map[string]any{
			"username": user.Username, "display_name": user.DisplayName,
			"department": user.Department, "agent_type": tok.AgentType, "tier": tok.Tier,
		})
		text = string(b)
	case "kb_search":
		q, _ := params.Arguments["query"].(string)
		var hits []kb.Hit
		s.st.Read(func(st *store.State) {
			hits = kb.Search(st, q, kb.MaxLevelForTier(tok.Tier), 10)
		})
		b, _ := json.Marshal(map[string]any{"hits": hits})
		text = string(b)
	case "kb_get":
		id, _ := params.Arguments["doc_id"].(string)
		var doc *store.KBDoc
		var found bool
		s.st.Read(func(st *store.State) {
			doc, found = kb.Get(st, id, kb.MaxLevelForTier(tok.Tier))
		})
		if !found {
			text, isErr = "document not found or not accessible at your tier", true
		} else {
			b, _ := json.Marshal(map[string]any{
				"id": doc.ID, "title": doc.Title, "version": doc.Version,
				"level": doc.Level.String(), "content": doc.Content,
			})
			text = string(b)
		}
	default:
		writeRPCError(w, req.ID, -32602, "unknown tool")
		return
	}

	// DLP scan of tool output before it enters the model's context.
	findings := s.det.Scan(text)
	level := guard.Classify(findings)
	if tok.Tier == store.TierBYOA && level >= store.LevelConfidential {
		red, _ := guard.Redact(text, findings)
		text = red
	}

	s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "tool_call", Action: params.Name, DataLevel: level.String(), Decision: "allow",
		Detail: map[string]string{"tool": params.Name}})
	writeRPCResult(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	})
}

func tierRank(t store.Tier) int {
	if t == store.TierManaged {
		return 2
	}
	return 1
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
}
