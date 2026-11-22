package mcp_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- F19: semantic index entries ----

type entriesResult struct {
	DocID   string `json:"doc_id"`
	Entries []struct {
		NodeID        string `json:"node_id"`
		Function      string `json:"function"`
		Relations     string `json:"relations"`
		Applicability string `json:"applicability"`
		Constraints   string `json:"constraints"`
		Tags          struct {
			Department string `json:"department"`
			Type       string `json:"type"`
			Importance string `json:"importance"`
		} `json:"tags"`
		Source struct {
			File    string `json:"file"`
			Chapter string `json:"chapter"`
			Pages   string `json:"pages"`
		} `json:"source"`
		GeneratedBy string `json:"generated_by"`
	} `json:"entries"`
}

func TestSemanticEntries(t *testing.T) {
	e := setup(t)
	text := resultText(t, e.callTool(t, e.byoaTok, "kb_catalog", map[string]any{}))
	var cat struct {
		Documents []struct {
			DocID string `json:"doc_id"`
			Title string `json:"title"`
		} `json:"documents"`
	}
	if err := json.Unmarshal([]byte(text), &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Documents) != 1 {
		t.Fatalf("catalog = %+v, want the single internal doc", cat.Documents)
	}
	docID := cat.Documents[0].DocID

	// BYOA reads the internal document's entries: one per section,
	// each with FRAS fields, tags and a source citation.
	text = resultText(t, e.callTool(t, e.byoaTok, "kb_entries", map[string]any{"doc_id": docID}))
	var res entriesResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("kb_entries result is not JSON: %v (%s)", err, text)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("got %d entries, want 3 (root + 2 chapters)", len(res.Entries))
	}
	for _, en := range res.Entries {
		if en.Function == "" || en.Relations == "" || en.Applicability == "" || en.Constraints == "" {
			t.Errorf("entry %s is missing FRAS fields: %+v", en.NodeID, en)
		}
		if en.Tags.Department == "" || en.Tags.Type == "" || en.Tags.Importance == "" {
			t.Errorf("entry %s is missing tag coordinates: %+v", en.NodeID, en.Tags)
		}
		if en.Source.File == "" || en.Source.Chapter == "" || en.Source.Pages == "" {
			t.Errorf("entry %s is missing its back-reference: %+v", en.NodeID, en.Source)
		}
		if en.GeneratedBy != "extractive" {
			t.Errorf("entry %s generated_by = %q, want extractive", en.NodeID, en.GeneratedBy)
		}
	}
	if res.Entries[1].Source.Chapter != "请假流程" || res.Entries[1].Tags.Importance != "medium" {
		t.Errorf("n1 entry = %+v", res.Entries[1])
	}
	if !strings.Contains(res.Entries[1].Function, "提前提交申请") {
		t.Errorf("n1 function should carry the chapter summary, got %q", res.Entries[1].Function)
	}

	// Unknown document: same "not accessible" shape as kb_structure.
	r := e.callTool(t, e.byoaTok, "kb_entries", map[string]any{"doc_id": "folder:nope:x.md"})
	if r.Result == nil || !r.Result.IsError {
		t.Fatalf("unknown doc entries: got %+v, want isError", r)
	}
}

func TestSemanticEntriesConfidentialHiddenFromBYOA(t *testing.T) {
	e := setup(t)
	text := resultText(t, e.callTool(t, e.mgdTok, "kb_catalog", map[string]any{}))
	var cat struct {
		Documents []struct {
			DocID string `json:"doc_id"`
			Title string `json:"title"`
		} `json:"documents"`
	}
	if err := json.Unmarshal([]byte(text), &cat); err != nil {
		t.Fatal(err)
	}
	var confID string
	for _, d := range cat.Documents {
		if d.Title == "机密资料" {
			confID = d.DocID
		}
	}
	if confID == "" {
		t.Fatal("confidential doc missing from managed catalog")
	}
	// BYOA cannot read the confidential document's entries at all.
	r := e.callTool(t, e.byoaTok, "kb_entries", map[string]any{"doc_id": confID})
	if r.Result == nil || !r.Result.IsError {
		t.Fatalf("BYOA entries of confidential doc: got %+v, want isError", r)
	}
	// Managed tier can.
	text = resultText(t, e.callTool(t, e.mgdTok, "kb_entries", map[string]any{"doc_id": confID}))
	var res entriesResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("managed kb_entries is not JSON: %v (%s)", err, text)
	}
	if len(res.Entries) == 0 {
		t.Fatalf("managed entries = %+v, want at least the root entry", res)
	}
	if res.Entries[0].Tags.Importance != "high" {
		t.Errorf("confidential importance = %q, want high", res.Entries[0].Tags.Importance)
	}
}

func TestToolsListIncludesKBEntries(t *testing.T) {
	e := setup(t)
	names := e.listToolNames(t, e.byoaTok)
	found := false
	for _, n := range names {
		if n == "kb_entries" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tools/list missing kb_entries: %v", names)
	}
}
