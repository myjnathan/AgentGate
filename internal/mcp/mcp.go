// Package mcp exposes AgentGate's capabilities as a remote MCP server
// (JSON-RPC 2.0 over HTTP, streamable-HTTP style) so any MCP-capable
// desktop agent can consume enterprise tools without a bespoke plugin.
// Tool-level authorization is enforced server-side per token tier, and
// every tool call is audited. Tool results pass a DLP scan before they
// are returned, because tool output is the second entry point for
// sensitive data into a model's context.
package mcp

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/kb"
	"github.com/myjnathan/agentgate/internal/store"
)

type Server struct {
	st    *store.Store
	auth  *auth.Service
	audit *audit.Log
	det   *guard.Detector
	// ToolMinTier: minimum tier required per tool (default BYOA).
	ToolMinTier map[string]store.Tier
}

func New(st *store.Store, au *auth.Service, al *audit.Log, det *guard.Detector) *Server {
	return &Server{st: st, auth: au, audit: al, det: det, ToolMinTier: map[string]store.Tier{
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
		writeRPCResult(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		s.callTool(w, req, tok, user)
	default:
		writeRPCError(w, req.ID, -32601, "method not found")
	}
}

func (s *Server) callTool(w http.ResponseWriter, req rpcRequest, tok *store.APIToken, user *store.User) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &params)

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
