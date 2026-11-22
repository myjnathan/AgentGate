package semidx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/store"
)

// ModelConfig is how a model is reached for semantic-entry generation.
// Endpoint is the base URL of a chat-completions-compatible API
// (normally this server's own gateway, so the generation request
// passes through classification routing and desensitization);
// Token is the gateway token the request authenticates with;
// Model is the model name sent upstream.
type ModelConfig struct {
	Endpoint string
	Token    string
	Model    string
}

// ModelGenerator writes semantic entries with a model. It is only a
// client of a chat-completions API: the gateway on the other end is
// what enforces the data-level routing, so generation obeys the same
// rules as any other model call (F17).
type ModelGenerator struct {
	cfg    ModelConfig
	client *http.Client
}

// NewModelGenerator builds a model generator; a nil client gets a
// 60-second-timeout default.
func NewModelGenerator(cfg ModelConfig, client *http.Client) *ModelGenerator {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.Model == "" {
		cfg.Model = "mock-eco"
	}
	return &ModelGenerator{cfg: cfg, client: client}
}

// Generate asks the model for FRAS entries for one document snapshot.
// The prompt carries only chapter titles, page ranges and extractive
// summaries — never full section text — so the model sees the index,
// not the document. Secret documents are refused outright: they get
// title-only entries from RegenerateDoc and must never be sent.
func (g *ModelGenerator) Generate(ctx context.Context, snap folderkb.DocSnapshot) ([]Entry, error) {
	if g.cfg.Endpoint == "" {
		return nil, fmt.Errorf("no semantic model endpoint configured")
	}
	if snap.Level == store.LevelSecret {
		return nil, fmt.Errorf("secret documents are never sent to a model")
	}
	prompt := BuildPrompt(snap)
	body, _ := json.Marshal(map[string]any{
		"model":       g.cfg.Model,
		"temperature": 0,
		"stream":      false,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	url := strings.TrimSuffix(g.cfg.Endpoint, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	}
	// Declare the document's level so the gateway routes the
	// generation request at that level (it can only raise, never
	// lower, the scanned level — see gateway.ChatCompletions).
	req.Header.Set("X-AgentGate-Data-Level", snap.Level.String())
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading model response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Only the status is recorded: the body may carry upstream
		// internals that must not land in our logs or audit trail.
		return nil, fmt.Errorf("model endpoint returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("model response is not a chat completion: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("model response has no choices")
	}
	return ParseModelEntries(snap, out.Choices[0].Message.Content)
}

// BuildPrompt renders the FRAS instruction for one document. Only the
// chapter tree (titles, page ranges, extractive summaries) goes in:
// the model judges relevance from the index, the full text stays in
// the folder. The instruction also fixes the output shape so the
// result can be validated mechanically.
func BuildPrompt(snap folderkb.DocSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "你是企业知识库的索引员。为下面这份文档的每个章节写一行 FRAS 语义条目，只输出 JSON，不要解释。\n文档：%s（%s，密级 %s）\n章节：\n", snap.Title, snap.RelPath, snap.Level.String())
	for _, ni := range walkNodes(snap.Tree) {
		n := ni.node
		fmt.Fprintf(&b, "- %s %s（第 %s 页）：%s\n", n.ID, n.Title, pagesOf(n), n.Summary)
	}
	b.WriteString("每条输出 {\"node_id\": \"n1\", \"function\": \"本节讲什么（职责）\", \"relations\": \"与哪些章节/制度相关\", \"applicability\": \"适用于谁/什么场景\", \"constraints\": \"硬约束（数字、期限、审批链）\", \"tags\": {\"department\": \"所属部门\", \"type\": \"文档类型\", \"importance\": \"low|medium|high|critical\"}}。\n")
	b.WriteString("要求：每项不超过 200 字；只写章节已有内容，不编造；tags.importance 按密级取：公开 low、内部 medium、机密 high、绝密 critical。\n")
	b.WriteString("只输出 {\"entries\": [...]} 一个 JSON 对象。")
	return b.String()
}

// modelEntry is the wire shape the model must return per node. The
// back-reference is deliberately absent: it is filled in from the
// index tree by ParseModelEntries, never taken from model text.
type modelEntry struct {
	NodeID        string `json:"node_id"`
	Function      string `json:"function"`
	Relations     string `json:"relations"`
	Applicability string `json:"applicability"`
	Constraints   string `json:"constraints"`
	Tags          Tags   `json:"tags"`
}

// ParseModelEntries decodes a model's JSON reply into entries for the
// snapshot's nodes. Unknown or duplicated node IDs are rejected;
// nodes the model skipped are left out (RegenerateDoc fills them
// extractively). Every returned entry is validated, and its
// back-reference is taken from the tree.
func ParseModelEntries(snap folderkb.DocSnapshot, content string) ([]Entry, error) {
	trimmed := strings.TrimSpace(content)
	// Tolerate a fenced code block around the JSON object.
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	var decoded struct {
		Entries []modelEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, fmt.Errorf("model output is not valid entries JSON: %w", err)
	}
	if len(decoded.Entries) == 0 {
		return nil, fmt.Errorf("model returned no entries")
	}
	nodes := map[string]*folderkb.Node{}
	for _, ni := range walkNodes(snap.Tree) {
		nodes[ni.node.ID] = ni.node
	}
	seen := map[string]bool{}
	now := time.Now()
	var out []Entry
	for _, me := range decoded.Entries {
		n, ok := nodes[me.NodeID]
		if !ok {
			return nil, fmt.Errorf("model entry names unknown node %q", me.NodeID)
		}
		if seen[me.NodeID] {
			return nil, fmt.Errorf("model entry duplicates node %q", me.NodeID)
		}
		seen[me.NodeID] = true
		e := Entry{
			DocID:         snap.ID,
			NodeID:        me.NodeID,
			Function:      me.Function,
			Relations:     me.Relations,
			Applicability: me.Applicability,
			Constraints:   me.Constraints,
			Tags:          me.Tags,
			Source:        SourceRef{File: snap.RelPath, Chapter: n.Title, Pages: pagesOf(n)},
			GeneratedBy:   GeneratedModel,
			GeneratedAt:   now,
		}
		if err := Validate(e); err != nil {
			return nil, fmt.Errorf("model entry for node %s failed validation: %w", me.NodeID, err)
		}
		out = append(out, e)
	}
	return out, nil
}
