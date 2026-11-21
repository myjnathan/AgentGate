package store_test

import (
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/store"
)

func TestRenderPromptSubstitutes(t *testing.T) {
	v := &store.PromptVersion{Version: 1,
		Content: "为{{department}}的{{name}}整理第{{week}}周周报，备注：{{note}}。",
		Arguments: []store.PromptArgument{
			{Name: "name", Required: true},
			{Name: "department", Required: true},
			{Name: "week", Required: true},
			{Name: "note", Required: false},
		}}
	out, err := store.RenderPrompt(v, map[string]any{
		"name": "李晓峰", "department": "研发部", "week": "2026-W47"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "为研发部的李晓峰整理第2026-W47周周报") {
		t.Fatalf("rendered text missing substitutions: %q", out)
	}
	// An optional argument left out renders as "" — no placeholder
	// syntax leaks to the model.
	if strings.Contains(out, "{{") {
		t.Fatalf("optional argument left a placeholder behind: %q", out)
	}
}

func TestRenderPromptValidation(t *testing.T) {
	v := &store.PromptVersion{Version: 1, Content: "你好，{{name}}。",
		Arguments: []store.PromptArgument{{Name: "name", Required: true}}}
	if _, err := store.RenderPrompt(v, map[string]any{}); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("missing required argument: got %v, want error naming name", err)
	}
	if _, err := store.RenderPrompt(v, map[string]any{"name": "li", "extra": "x"}); err == nil || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("unknown argument: got %v, want error naming extra", err)
	}
	if _, err := store.RenderPrompt(v, map[string]any{"name": 42}); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("non-string argument: got %v, want error naming name", err)
	}
}

func TestValidateTemplateVersion(t *testing.T) {
	args := []store.PromptArgument{{Name: "name", Required: true}}
	if err := store.ValidateTemplateVersion("你好，{{name}}。", args); err != nil {
		t.Fatalf("valid version: %v", err)
	}
	if err := store.ValidateTemplateVersion("你好，{{unknown}}。", args); err == nil {
		t.Fatal("undeclared placeholder must be rejected")
	}
	if err := store.ValidateTemplateVersion("你好，{{name}。", args); err == nil {
		t.Fatal("unclosed placeholder must be rejected")
	}
	if err := store.ValidateTemplateVersion("你好，{{Name}}。", args); err == nil {
		t.Fatal("malformed placeholder (capital) must be rejected")
	}
	if err := store.ValidateTemplateVersion("x", []store.PromptArgument{{Name: "a"}, {Name: "a"}}); err == nil {
		t.Fatal("duplicate argument must be rejected")
	}
	if err := store.ValidateTemplateVersion("x", []store.PromptArgument{{Name: "Bad"}}); err == nil {
		t.Fatal("non-snake_case argument must be rejected")
	}
	if err := store.ValidateTemplateVersion("", args); err == nil {
		t.Fatal("empty content must be rejected")
	}
	if err := store.ValidateTemplateVersion(strings.Repeat("x", store.MaxPromptContentBytes+1), nil); err == nil {
		t.Fatal("oversize content must be rejected")
	}
	many := make([]store.PromptArgument, store.MaxPromptArguments+1)
	for i := range many {
		many[i] = store.PromptArgument{Name: "a" + string(rune('a'+i%26)) + string(rune('0'+i/26))}
	}
	if err := store.ValidateTemplateVersion("x", many); err == nil {
		t.Fatal("too many arguments must be rejected")
	}
}

func TestPromptPublishedResolution(t *testing.T) {
	tpl := &store.PromptTemplate{
		CurrentVersion: 2, PublishedVersion: 1,
		Versions: []store.PromptVersion{
			{Version: 1, Content: "v1"},
			{Version: 2, Content: "v2"},
		},
	}
	if got := tpl.Published(); got == nil || got.Content != "v1" {
		t.Fatalf("Published() = %+v, want v1", got)
	}
	if got := tpl.Version(2); got == nil || got.Content != "v2" {
		t.Fatalf("Version(2) = %+v, want v2", got)
	}
	if got := tpl.Version(9); got != nil {
		t.Fatalf("Version(9) = %+v, want nil", got)
	}
	tpl.PublishedVersion = 0
	if got := tpl.Published(); got != nil {
		t.Fatalf("unpublished Published() = %+v, want nil", got)
	}
}

func TestPromptInScope(t *testing.T) {
	tpl := &store.PromptTemplate{Scope: "研发部,注册部"}
	if !tpl.InScope("研发部") || !tpl.InScope("注册部") {
		t.Fatal("listed departments must be in scope")
	}
	if tpl.InScope("财务部") {
		t.Fatal("unlisted department must be out of scope")
	}
	if !(&store.PromptTemplate{Scope: "*"}).InScope("财务部") {
		t.Fatal("\"*\" scope must match every department")
	}
}
