package semidx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/myjnathan/agentgate/internal/store"
)

func TestBuildPromptOnlyCarriesTheIndex(t *testing.T) {
	prompt := BuildPrompt(testSnapshot(store.LevelInternal))
	for _, want := range []string{"请假流程", "加班管理", "第 1 页", "第 2 页", "FRAS", `"entries"`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// The prompt is built from the tree (titles + summaries) only;
	// there is no section text in a snapshot to leak in the first place.
	if strings.Contains(prompt, "年假每年 5 天起") {
		t.Errorf("prompt must not contain section body text")
	}
}

func TestParseModelEntries(t *testing.T) {
	snap := testSnapshot(store.LevelInternal)
	content := `{"entries": [
		{"node_id": "n1", "function": "处理员工请假申请与审批。", "relations": "关联考勤打卡。", "applicability": "适用于全体员工。", "constraints": "年假每年 5 天起。", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}},
		{"node_id": "n2", "function": "管理加班报备与调休。", "relations": "关联请假流程。", "applicability": "适用于全体员工。", "constraints": "每月加班不得超过 36 小时。", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}}
	]}`
	entries, err := ParseModelEntries(snap, content)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	e := entries[0]
	if e.GeneratedBy != GeneratedModel {
		t.Errorf("generated_by = %q, want model", e.GeneratedBy)
	}
	// The back-reference comes from the tree, not from the model text:
	// the model never supplied file/chapter/pages.
	if e.Source != (SourceRef{File: "考勤与请假制度.md", Chapter: "请假流程", Pages: "1"}) {
		t.Errorf("back-reference = %+v, want tree-derived citation", e.Source)
	}
	// A fenced code block around the JSON is tolerated.
	fenced := "```json\n" + content + "\n```"
	if _, err := ParseModelEntries(snap, fenced); err != nil {
		t.Errorf("fenced JSON should parse: %v", err)
	}
	for name, bad := range map[string]string{
		"not json":         `hello`,
		"unknown node":     `{"entries": [{"node_id": "n9", "function": "x", "relations": "x", "applicability": "x", "constraints": "x", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}}]}`,
		"duplicated node":  `{"entries": [{"node_id": "n1", "function": "x", "relations": "x", "applicability": "x", "constraints": "x", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}}, {"node_id": "n1", "function": "y", "relations": "y", "applicability": "y", "constraints": "y", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}}]}`,
		"empty FRAS field": `{"entries": [{"node_id": "n1", "function": "", "relations": "x", "applicability": "x", "constraints": "x", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}}]}`,
		"bad importance":   `{"entries": [{"node_id": "n1", "function": "x", "relations": "x", "applicability": "x", "constraints": "x", "tags": {"department": "人力部", "type": "制度", "importance": "urgent"}}]}`,
		"no entries":       `{"entries": []}`,
	} {
		if _, err := ParseModelEntries(snap, bad); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
	// "model invents source" must fail validation... unless the extra
	// field is ignored. json.Unmarshal ignores unknown fields, so the
	// invented source is dropped and the entry is valid — the tree
	// back-reference wins either way. Assert the tree citation.
	entries, err = ParseModelEntries(snap, `{"entries": [{"node_id": "n1", "function": "x", "relations": "x", "applicability": "x", "constraints": "x", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}, "source": {"file": "fake.md", "chapter": "伪造章节", "pages": "99"}}]}`)
	if err != nil {
		t.Fatalf("entry with extra source field should still parse: %v", err)
	}
	if entries[0].Source.Chapter != "请假流程" {
		t.Errorf("model-supplied citation must never win: %+v", entries[0].Source)
	}
}

func TestModelGeneratorWithFakeServer(t *testing.T) {
	var gotLevel, gotAuth, gotModel string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotLevel = r.Header.Get("X-AgentGate-Data-Level")
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		if len(req.Messages) == 0 || !strings.Contains(req.Messages[0].Content, "请假流程") {
			t.Errorf("model request should carry the chapter tree")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{
			{"message": map[string]any{"content": `{"entries": [
				{"node_id": "n0", "function": "考勤与请假制度总则。", "relations": "包含章节：请假流程、加班管理。", "applicability": "适用于星河精密全体员工。", "constraints": "由人力部负责解释。", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}},
				{"node_id": "n1", "function": "处理员工请假申请与审批。", "relations": "关联考勤打卡。", "applicability": "适用于全体员工。", "constraints": "年假每年 5 天起。", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}},
				{"node_id": "n2", "function": "管理加班报备与调休。", "relations": "关联请假流程。", "applicability": "适用于全体员工。", "constraints": "每月加班不得超过 36 小时。", "tags": {"department": "人力部", "type": "制度", "importance": "medium"}}
			]}`}},
		}})
	}))
	defer srv.Close()

	gen := NewModelGenerator(ModelConfig{Endpoint: srv.URL, Token: "tok-test", Model: "mock-eco"}, nil)
	entries, err := gen.Generate(context.Background(), testSnapshot(store.LevelInternal))
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if gotLevel != "internal" {
		t.Errorf("data-level header = %q, want internal (routes the request at the doc level)", gotLevel)
	}
	if gotAuth != "Bearer tok-test" {
		t.Errorf("auth header = %q, want bearer token", gotAuth)
	}
	if gotModel != "mock-eco" {
		t.Errorf("model = %q, want mock-eco", gotModel)
	}
	if entries[0].Tags.Type != "制度" {
		t.Errorf("model-supplied tag should be kept, got %+v", entries[0].Tags)
	}
}

func TestModelGeneratorEchoFallsBack(t *testing.T) {
	// A mock LLM echoes the prompt back: the reply is not entries
	// JSON, so Generate must fail (the caller falls back to
	// extractive entries) rather than indexing the echo.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{
			{"message": map[string]any{"content": req.Messages[0].Content}},
		}})
	}))
	defer srv.Close()
	gen := NewModelGenerator(ModelConfig{Endpoint: srv.URL}, nil)
	if _, err := gen.Generate(context.Background(), testSnapshot(store.LevelInternal)); err == nil {
		t.Errorf("echoed prompt should not parse as entries")
	}
}

func TestModelGeneratorRefusesSecret(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	gen := NewModelGenerator(ModelConfig{Endpoint: srv.URL}, nil)
	if _, err := gen.Generate(context.Background(), testSnapshot(store.LevelSecret)); err == nil {
		t.Errorf("secret document must never be sent to a model")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("secret document caused %d model requests, want 0", calls)
	}
}

func TestModelGeneratorUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error": "upstream exploded with secrets"}`))
	}))
	defer srv.Close()
	gen := NewModelGenerator(ModelConfig{Endpoint: srv.URL}, nil)
	_, err := gen.Generate(context.Background(), testSnapshot(store.LevelInternal))
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("upstream failure should surface as HTTP 502, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "secrets") {
		t.Errorf("upstream body must not be echoed into the error: %v", err)
	}
}
