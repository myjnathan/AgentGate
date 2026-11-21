// Package federation federates upstream MCP servers behind AgentGate's
// own /mcp endpoint (backlog F6, borrowing IBM ContextForge's
// federation model): each registered upstream contributes its tools
// under a "<namespace>.<tool>" name, and calls are forwarded over
// JSON-RPC 2.0. The registry only moves bytes; tier authorization,
// data-level gating and DLP live in the calling layer (internal/mcp),
// so a federated tool can never bypass platform policy. Upstream
// credentials are decrypted only in memory for the forwarded request.
package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// Tool is one upstream tool as advertised by tools/list.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// CacheTTL bounds how stale a tools/list answer may be. A failed
// refresh falls back to the stale entry (if any) so one sick upstream
// never takes the whole federated list down with it. It is a variable
// so tests can shrink it; production code leaves the default.
var CacheTTL = 60 * time.Second

// Registry holds upstream servers and a short-lived tools cache.
type Registry struct {
	st    *store.Store
	box   *secretbox.Box
	http  *http.Client
	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	tools     []Tool
	fetchedAt time.Time
}

func New(st *store.Store, box *secretbox.Box) *Registry {
	return &Registry{st: st, box: box,
		http:  &http.Client{Timeout: 10 * time.Second},
		cache: map[string]cacheEntry{}}
}

// Upstream returns a copy of the registered upstream with this
// namespace, or false when there is none.
func (r *Registry) Upstream(name string) (*store.UpstreamServer, bool) {
	var found *store.UpstreamServer
	r.st.Read(func(st *store.State) {
		for _, up := range st.Upstreams {
			if up.Name == name {
				cp := *up
				found = &cp
			}
		}
	})
	return found, found != nil
}

// Enabled returns copies of all enabled upstreams.
func (r *Registry) Enabled() []*store.UpstreamServer {
	var out []*store.UpstreamServer
	r.st.Read(func(st *store.State) {
		for _, up := range st.Upstreams {
			if up.Enabled {
				cp := *up
				out = append(out, &cp)
			}
		}
	})
	return out
}

// ListTools returns the upstream's tools, from cache when fresh.
// A refresh failure returns the stale cached tools when available;
// only a cold failure is an error.
func (r *Registry) ListTools(ctx context.Context, up *store.UpstreamServer) ([]Tool, error) {
	r.mu.Lock()
	ent, ok := r.cache[up.ID]
	r.mu.Unlock()
	if ok && time.Since(ent.fetchedAt) < CacheTTL {
		return ent.tools, nil
	}
	tools, err := r.listToolsFresh(ctx, up)
	if err != nil {
		if ok {
			return ent.tools, nil
		}
		return nil, err
	}
	r.mu.Lock()
	r.cache[up.ID] = cacheEntry{tools: tools, fetchedAt: time.Now()}
	r.mu.Unlock()
	return tools, nil
}

// ListToolsFresh bypasses the cache; used by the admin test endpoint
// so a connectivity check reports the live state.
func (r *Registry) ListToolsFresh(ctx context.Context, up *store.UpstreamServer) ([]Tool, error) {
	tools, err := r.listToolsFresh(ctx, up)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache[up.ID] = cacheEntry{tools: tools, fetchedAt: time.Now()}
	r.mu.Unlock()
	return tools, nil
}

func (r *Registry) listToolsFresh(ctx context.Context, up *store.UpstreamServer) ([]Tool, error) {
	var res struct {
		Tools []Tool `json:"tools"`
	}
	if err := r.callRPC(ctx, up, "tools/list", nil, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool forwards one tools/call to the upstream and returns the
// concatenated text content. isError mirrors the upstream's isError
// flag; transport or protocol failures are Go errors.
func (r *Registry) CallTool(ctx context.Context, up *store.UpstreamServer, name string, args map[string]any) (string, bool, error) {
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := r.callRPC(ctx, up, "tools/call",
		map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return "", false, err
	}
	var text string
	for _, c := range res.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	return text, res.IsError, nil
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// callRPC performs one JSON-RPC 2.0 request against an upstream MCP
// server (streamable-HTTP style POST) and decodes the result. The
// upstream error message is never propagated to callers: it may
// contain upstream-internal detail.
func (r *Registry) callRPC(ctx context.Context, up *store.UpstreamServer, method string, params any, out any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if up.AuthTokenEnc != "" {
		tok, err := r.box.Decrypt(up.AuthTokenEnc)
		if err != nil {
			return fmt.Errorf("decrypt upstream token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream %s returned HTTP %d", up.Name, resp.StatusCode)
	}
	var rr rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return fmt.Errorf("decode upstream response: %w", err)
	}
	if rr.Error != nil {
		return fmt.Errorf("upstream %s returned JSON-RPC error %d", up.Name, rr.Error.Code)
	}
	if out != nil {
		if err := json.Unmarshal(rr.Result, out); err != nil {
			return fmt.Errorf("decode upstream result: %w", err)
		}
	}
	return nil
}
