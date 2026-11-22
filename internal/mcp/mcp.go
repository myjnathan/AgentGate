// Package mcp exposes AgentGate's capabilities as a remote MCP server
// (JSON-RPC 2.0 over HTTP, streamable-HTTP style) so any MCP-capable
// desktop agent can consume enterprise tools without a bespoke plugin.
// Tool-level authorization is enforced server-side per token tier, and
// every tool call is audited. Tool results pass a DLP scan before they
// are returned, because tool output is the second entry point for
// sensitive data into a model's context.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/kb"
	"github.com/myjnathan/agentgate/internal/knowpack"
	"github.com/myjnathan/agentgate/internal/otel"
	"github.com/myjnathan/agentgate/internal/restadapter"
	"github.com/myjnathan/agentgate/internal/semidx"
	"github.com/myjnathan/agentgate/internal/store"
)

type Server struct {
	st    *store.Store
	auth  *auth.Service
	audit *audit.Log
	det   *guard.Detector
	fed   *federation.Registry
	rest  *restadapter.Adapter
	fk    *folderkb.Registry
	semix *semidx.Index
	// tracer is the OpenTelemetry exporter (F16); nil or disabled
	// means no tracing. Spans carry metadata only — never tool
	// arguments or results (see docs/opentelemetry.md).
	tracer *otel.Tracer
	// ToolMinTier: minimum tier required per tool (default BYOA).
	ToolMinTier map[string]store.Tier
}

// SetTracer wires the OpenTelemetry exporter (backlog F16).
func (s *Server) SetTracer(tr *otel.Tracer) {
	s.tracer = tr
}

// SetFolderKB wires the folder knowledge-base registry (backlog
// F18) used by kb_catalog / kb_structure / kb_section and by the
// folder half of kb_search.
func (s *Server) SetFolderKB(fk *folderkb.Registry) {
	s.fk = fk
}

// SetSemanticIndex wires the semantic index (backlog F19) used by
// the kb_entries tool.
func (s *Server) SetSemanticIndex(ix *semidx.Index) {
	s.semix = ix
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
		Description: "Search the enterprise knowledge base by keywords. Covers seeded documents and folder-indexed files; returns cited excerpts (file, section, pages). Call repeatedly with refined keywords, then use kb_get for a seeded document or kb_section for a folder document.",
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
		Name:        "kb_catalog",
		Description: "List the folder-indexed knowledge-base documents visible at your tier (the Wiki home): title, source folder, data level, page count and top-level sections. Start here, then read a document's semantic entries with kb_entries to judge relevance, then kb_structure and kb_section to drill down.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		Name:        "kb_entries",
		Description: "Read the FRAS semantic index entries of one folder document (the catalog layer): one entry per section with function, relations, applicability and hard constraints, tag coordinates (department/type/importance), and a source citation (file + section + pages). Read these before kb_structure when judging whether a document is relevant.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"doc_id": map[string]any{"type": "string"},
		}, "required": []string{"doc_id"}},
	},
	{
		Name:        "kb_structure",
		Description: "Read one folder document's section tree: node id, title, page range and summary per section (no full text). Pick a node, then read it with kb_section.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"doc_id": map[string]any{"type": "string"},
		}, "required": []string{"doc_id"}},
	},
	{
		Name:        "kb_section",
		Description: "Read the full text of one section of a folder document by node id (see kb_structure). The result is cited as file + section + pages.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"doc_id":  map[string]any{"type": "string"},
			"node_id": map[string]any{"type": "string"},
		}, "required": []string{"doc_id", "node_id"}},
	},
	{
		Name:        "kb_pack_list",
		Description: "List the published curated knowledge packages visible at your tier: one package per work problem (onboarding, vendor qualification, …), each with its purpose, level, source count and version. A package is the curated entry point — read it with kb_pack_get, then drill down to the source sections with kb_structure and kb_section.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		Name:        "kb_pack_get",
		Description: "Read one published knowledge package by name or id: its synthesis (purpose, evidence usage, boundaries and conclusions, every conclusion backed by a citation), its source list, and the published synthesis of the packages it references. Citations name file + section + pages; use kb_section to read the cited original.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "package name or id"},
		}, "required": []string{"name"}},
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
			"capabilities":    map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agentgate", "version": "0.1.0"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		writeRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": s.listTools(r.Context(), tok)})
	case "tools/call", "prompts/get":
		// Traced calls (F16): the response is buffered so the
		// deferred span end can tell success from a JSON-RPC error
		// (MCP errors ride on HTTP 200).
		s.serveTraced(w, r, req, tok, user)
	case "prompts/list":
		writeRPCResult(w, req.ID, map[string]any{"prompts": s.listPrompts(tok, user)})
	default:
		writeRPCError(w, req.ID, -32601, "method not found")
	}
}

// rpcRecorder buffers one MCP JSON-RPC response. WriteHeader is
// swallowed and replayed by flush; responses are small single JSON
// objects, never a stream.
type rpcRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *rpcRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *rpcRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *rpcRecorder) flush() {
	r.ResponseWriter.WriteHeader(r.status)
	_, _ = r.ResponseWriter.Write(r.body.Bytes())
}

func toolNameFromParams(params json.RawMessage) string {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	return p.Name
}

// serveTraced runs a tool or prompt call behind one server span.
func (s *Server) serveTraced(w http.ResponseWriter, r *http.Request, req rpcRequest, tok *store.APIToken, user *store.User) {
	rec := &rpcRecorder{ResponseWriter: w}
	span := s.startTrace(r, req, tok, user)
	defer func() {
		s.endTrace(span, req, rec)
		rec.flush()
	}()
	switch req.Method {
	case "tools/call":
		s.callTool(rec, r.Context(), req, tok, user)
	case "prompts/get":
		s.getPrompt(rec, req, tok, user)
	}
}

func (s *Server) startTrace(r *http.Request, req rpcRequest, tok *store.APIToken, user *store.User) *otel.Span {
	if s.tracer == nil {
		return nil
	}
	parent, _ := otel.ParseTraceParent(r.Header.Get("traceparent"))
	return s.tracer.Start("mcp."+strings.Replace(req.Method, "/", ".", 1), parent,
		otel.StringAttr("agentgate.actor_id", user.ID),
		otel.StringAttr("agentgate.agent_type", tok.AgentType),
		otel.StringAttr("agentgate.tier", string(tok.Tier)),
		otel.StringAttr("agentgate.method", req.Method),
		otel.StringAttr("agentgate.tool", toolNameFromParams(req.Params)))
}

// endTrace finishes the span from the buffered JSON-RPC response:
// a body with an "error" member is an error span, anything else is
// OK. The tool name is metadata; arguments and results are never
// span attributes.
func (s *Server) endTrace(span *otel.Span, req rpcRequest, rec *rpcRecorder) {
	if span == nil {
		return
	}
	decision, status, msg := "allow", 1, ""
	var rpcErrCode int64
	var body struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.body.Bytes(), &body); err == nil && body.Error != nil {
		decision, status, msg = "error", 2, "rpc_error"
		rpcErrCode = int64(body.Error.Code)
	}
	tool := toolNameFromParams(req.Params)
	attrs := []otel.Attr{
		otel.StringAttr("agentgate.tool", tool),
		otel.StringAttr("agentgate.decision", decision),
	}
	if body.Error != nil {
		attrs = append(attrs, otel.IntAttr("agentgate.rpc_error_code", rpcErrCode))
	}
	span.SetAttrs(attrs...)
	span.End(status, msg)
	if tr := s.tracer; tr != nil {
		switch req.Method {
		case "tools/call":
			tr.AddCounter("agentgate.mcp.tool_calls", 1, map[string]string{"tool": tool, "decision": decision})
		case "prompts/get":
			tr.AddCounter("agentgate.mcp.prompt_gets", 1, map[string]string{"decision": decision})
		}
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
		maxLevel := kb.MaxLevelForTier(tok.Tier)
		var hits []folderkb.Hit
		s.st.Read(func(st *store.State) {
			for _, h := range kb.Search(st, q, maxLevel, 10) {
				hits = append(hits, folderkb.Hit{
					DocID: h.DocID, Title: h.Title, Level: h.Level,
					Score: h.Score, Excerpt: h.Excerpt, Source: "kb", Version: h.Version,
				})
			}
		})
		// Folder-indexed files (backlog F18) search alongside the
		// seeded documents, under the same level gate.
		if s.fk != nil {
			hits = append(hits, s.fk.Search(q, maxLevel, 10)...)
		}
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].Score != hits[j].Score {
				return hits[i].Score > hits[j].Score
			}
			return hits[i].DocID < hits[j].DocID
		})
		if len(hits) > 10 {
			hits = hits[:10]
		}
		b, _ := json.Marshal(map[string]any{"hits": hits})
		text = string(b)
	case "kb_catalog":
		entries := []folderkb.CatalogEntry{}
		if s.fk != nil {
			entries = s.fk.Catalog(kb.MaxLevelForTier(tok.Tier))
		}
		b, _ := json.Marshal(map[string]any{"documents": entries})
		text = string(b)
	case "kb_entries":
		id, _ := params.Arguments["doc_id"].(string)
		var entries []semidx.Entry
		var found bool
		if s.semix != nil {
			entries, found = s.semix.Entries(id, kb.MaxLevelForTier(tok.Tier))
		}
		if !found {
			text, isErr = "document not found, has no semantic entries, or is not accessible at your tier", true
		} else {
			if entries == nil {
				entries = []semidx.Entry{}
			}
			b, _ := json.Marshal(map[string]any{"doc_id": id, "entries": entries})
			text = string(b)
		}
	case "kb_structure":
		id, _ := params.Arguments["doc_id"].(string)
		var tree *folderkb.Node
		var found bool
		if s.fk != nil {
			tree, found = s.fk.Structure(id, kb.MaxLevelForTier(tok.Tier))
		}
		if !found {
			text, isErr = "document not found or not accessible at your tier", true
		} else {
			b, _ := json.Marshal(map[string]any{
				"doc_id": id, "title": tree.Title, "pages": tree.EndPage, "tree": tree,
			})
			text = string(b)
		}
	case "kb_section":
		id, _ := params.Arguments["doc_id"].(string)
		nodeID, _ := params.Arguments["node_id"].(string)
		var sec *folderkb.Section
		var file string
		var found bool
		if s.fk != nil {
			sec, file, found = s.fk.Section(id, nodeID, kb.MaxLevelForTier(tok.Tier))
		}
		if !found {
			text, isErr = "document or section not found or not accessible at your tier", true
		} else {
			pages := strconv.Itoa(sec.StartPage)
			if sec.EndPage > sec.StartPage {
				pages = strconv.Itoa(sec.StartPage) + "-" + strconv.Itoa(sec.EndPage)
			}
			b, _ := json.Marshal(map[string]any{
				"doc_id": id, "file": file, "section": sec.Title,
				"pages": pages, "text": sec.Text,
			})
			text = string(b)
		}
	case "kb_pack_list":
		b, _ := json.Marshal(map[string]any{"packages": s.listKnowledgePackages(tok, user)})
		text = string(b)
	case "kb_pack_get":
		name, _ := params.Arguments["name"].(string)
		pkg, found := s.getKnowledgePackage(tok, user, name)
		if !found {
			text, isErr = "package not found or not accessible at your tier", true
		} else {
			b, _ := json.Marshal(pkg)
			text = string(b)
		}
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

// ---- F22: curated knowledge packages ----

type packListEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Level       string `json:"level"`
	Scope       string `json:"scope"`
	Version     int    `json:"published_version"`
	Sources     int    `json:"sources"`
	References  int    `json:"references"`
}

// listKnowledgePackages advertises the published packages the caller
// may use: status published, level at or below the caller's tier cap,
// and the caller's department in scope. Unpublished, above-tier and
// out-of-scope packages are indistinguishable from unknown ones.
func (s *Server) listKnowledgePackages(tok *store.APIToken, user *store.User) []packListEntry {
	maxLevel := kb.MaxLevelForTier(tok.Tier)
	out := []packListEntry{}
	s.st.Read(func(st *store.State) {
		for _, p := range st.KnowledgePackages {
			pub := p.Published()
			if pub == nil {
				continue
			}
			lvl, ok := store.ParseDataLevel(pub.Level)
			if !ok || lvl > maxLevel {
				continue
			}
			if !p.InScope(user.Department) {
				continue
			}
			out = append(out, packListEntry{ID: p.ID, Name: p.Name,
				Description: p.Description, Level: pub.Level, Scope: p.Scope,
				Version: p.PublishedVersion, Sources: len(pub.Sources),
				References: len(pub.References)})
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type packSourceView struct {
	DocID  string `json:"doc_id"`
	NodeID string `json:"node_id,omitempty"`
	File   string `json:"file,omitempty"`
	Title  string `json:"title,omitempty"`
	Level  string `json:"level,omitempty"`
}

type packReferenceView struct {
	Name      string                  `json:"name"`
	Level     string                  `json:"level"`
	Synthesis string                  `json:"synthesis"`
	Citations []store.PackageCitation `json:"citations"`
}

type packGetResult struct {
	ID                 string                  `json:"id"`
	Name               string                  `json:"name"`
	Description        string                  `json:"description"`
	Level              string                  `json:"level"`
	Scope              string                  `json:"scope"`
	Version            int                     `json:"published_version"`
	Synthesis          string                  `json:"synthesis"`
	Citations          []store.PackageCitation `json:"citations"`
	Sources            []packSourceView        `json:"sources"`
	References         []packReferenceView     `json:"references"`
	WithheldReferences []string                `json:"withheld_references,omitempty"`
}

// getKnowledgePackage returns one published package for the caller,
// with its synthesis, citations and source list, plus the published
// synthesis of every package it references. A reference whose package
// was withdrawn or republished higher since this package was
// published is withheld by name instead of served — the package
// itself stays readable, the stale reference does not leak through.
func (s *Server) getKnowledgePackage(tok *store.APIToken, user *store.User, nameOrID string) (*packGetResult, bool) {
	var pkg *store.KnowledgePackage
	var pkgs map[string]*store.KnowledgePackage
	s.st.Read(func(st *store.State) {
		pkgs = st.KnowledgePackages
		for _, p := range st.KnowledgePackages {
			if p.ID == nameOrID || p.Name == nameOrID {
				cp := *p
				pkg = &cp
			}
		}
	})
	if pkg == nil {
		return nil, false
	}
	pub := pkg.Published()
	if pub == nil || !pkg.InScope(user.Department) {
		return nil, false
	}
	lvl, ok := store.ParseDataLevel(pub.Level)
	if !ok || lvl > kb.MaxLevelForTier(tok.Tier) {
		return nil, false
	}
	res := &packGetResult{ID: pkg.ID, Name: pkg.Name, Description: pkg.Description,
		Level: pub.Level, Scope: pkg.Scope, Version: pkg.PublishedVersion,
		Synthesis: pub.Content, Citations: pub.Citations,
		Sources: []packSourceView{}, References: []packReferenceView{}}
	for _, src := range pub.Sources {
		v := packSourceView{DocID: src.DocID, NodeID: src.NodeID}
		if s.fk != nil {
			if snap, ok := s.fk.Snapshot(src.DocID); ok {
				v.File, v.Title, v.Level = snap.RelPath, snap.Title, snap.Level.String()
			}
		}
		res.Sources = append(res.Sources, v)
	}
	for _, ref := range pub.References {
		name, rlevel, synthesis, cits, ok := knowpack.PublishedSynthesis(ref, pkgs, lvl)
		if !ok {
			withheld := ref
			s.st.Read(func(st *store.State) {
				if p, ok := st.KnowledgePackages[ref]; ok {
					withheld = p.Name
				}
			})
			res.WithheldReferences = append(res.WithheldReferences, withheld)
			continue
		}
		res.References = append(res.References, packReferenceView{
			Name: name, Level: rlevel, Synthesis: synthesis, Citations: cits})
	}
	return res, true
}

// ---- F8: prompt templates ----

type promptDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Arguments   []store.PromptArgument `json:"arguments"`
}

// listPrompts advertises the published templates the caller may use:
// status published, tier sufficient, and the caller's department in
// scope. Unpublished templates are never listed — an agent cannot
// discover a draft the way it cannot discover a hidden tool.
func (s *Server) listPrompts(tok *store.APIToken, user *store.User) []promptDef {
	var out []promptDef
	s.st.Read(func(st *store.State) {
		for _, t := range st.Prompts {
			if t.Status != "published" || t.Published() == nil {
				continue
			}
			if tierRank(tok.Tier) < tierRank(t.MinTier) {
				continue
			}
			if !t.InScope(user.Department) {
				continue
			}
			out = append(out, promptDef{Name: t.Name, Description: t.Description,
				Arguments: t.Published().Arguments})
		}
	})
	return out
}

// getPrompt renders a published prompt template with the caller's
// arguments. The version selector follows Portkey's references with
// one governance tightening: agents only ever receive the published
// version. "" / "@latest" / "@published" mean the published version;
// a numeric version must be the published one, otherwise the call
// fails — an unpublished version is never served, whatever the
// selector. The same gates as a tool call apply: tier authorization,
// then a DLP scan of the arguments (banned content is blocked, and
// arguments classified above the template's MaxDataLevel are never
// rendered), then a DLP scan of the rendered text with BYOA
// redaction before it enters the model's context.
func (s *Server) getPrompt(w http.ResponseWriter, req rpcRequest, tok *store.APIToken, user *store.User) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Version   string         `json:"version"`
	}
	_ = json.Unmarshal(req.Params, &params)

	var tpl *store.PromptTemplate
	s.st.Read(func(st *store.State) {
		for _, t := range st.Prompts {
			if t.Name == params.Name {
				cp := *t
				tpl = &cp
			}
		}
	})
	// Unpublished or unknown templates are indistinguishable to the
	// caller: a draft's existence must not leak through prompts/get.
	if tpl == nil || tpl.Status != "published" || tpl.Published() == nil {
		writeRPCError(w, req.ID, -32602, "unknown prompt")
		return
	}
	deny := func(reason string) {
		s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "prompt_get_denied", Decision: "block",
			Detail: map[string]string{"prompt": tpl.Name, "reason": reason}})
	}
	if tierRank(tok.Tier) < tierRank(tpl.MinTier) {
		deny("tier_insufficient")
		writeRPCError(w, req.ID, -32003, "prompt not permitted for this agent tier")
		return
	}
	if !tpl.InScope(user.Department) {
		writeRPCError(w, req.ID, -32602, "unknown prompt")
		return
	}
	version := tpl.PublishedVersion
	switch sel := params.Version; sel {
	case "", "@latest", "@published":
		// Published version (already resolved above).
	case "@draft":
		writeRPCError(w, req.ID, -32602, "version not published")
		return
	default:
		n, err := strconv.Atoi(sel)
		if err != nil || n < 1 {
			writeRPCError(w, req.ID, -32602, "invalid version")
			return
		}
		if tpl.Version(n) == nil || n != tpl.PublishedVersion {
			writeRPCError(w, req.ID, -32602, "version not published")
			return
		}
		version = n
	}
	ver := tpl.Version(version)

	argJSON, _ := json.Marshal(params.Arguments)
	findings := s.det.Scan(string(argJSON))
	for _, f := range findings {
		if f.Category == guard.CatBanned {
			deny("banned_content")
			writeRPCError(w, req.ID, -32004, "request blocked by data policy")
			return
		}
	}
	if lvl := guard.Classify(findings); lvl > tpl.MaxDataLevel {
		deny("data_level_exceeds_template")
		writeRPCError(w, req.ID, -32004, "request blocked by data policy")
		return
	}

	text, err := store.RenderPrompt(ver, params.Arguments)
	if err != nil {
		writeRPCError(w, req.ID, -32602, err.Error())
		return
	}

	// DLP scan of the rendered prompt before it enters the model's
	// context (the template text itself is admin-authored, but
	// argument values come from the caller).
	findings = s.det.Scan(text)
	level := guard.Classify(findings)
	if tok.Tier == store.TierBYOA && level >= store.LevelConfidential {
		red, _ := guard.Redact(text, findings)
		text = red
	}

	s.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "prompt_get", Action: "prompts/get", DataLevel: level.String(), Decision: "allow",
		Detail: map[string]string{"prompt": tpl.Name, "version": strconv.Itoa(version)}})
	writeRPCResult(w, req.ID, map[string]any{
		"description": tpl.Description,
		"messages": []map[string]any{{
			"role":    "user",
			"content": map[string]any{"type": "text", "text": text},
		}},
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
