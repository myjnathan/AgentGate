package sidecar

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// slowReader yields one byte per Read, so every placeholder is split
// across reads — the holdback must still restore it whole.
type slowReader struct{ b []byte }

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	p[0] = r.b[0]
	r.b = r.b[1:]
	return 1, nil
}

func TestRestoreStreamSplitPlaceholders(t *testing.T) {
	mapping := map[string]string{
		"<PII_1>":        "lixiaofeng@xinghe.example",
		"<CREDENTIAL_1>": "sk-abcdefghijklmnop1234",
	}
	in := "data: {\"content\":\"hi <PII_1> and <CREDENTIAL_1>\"}\n\n" +
		"data: {\"content\":\"again <PII_1>\"}\n\ndata: [DONE]\n\n"
	var out bytes.Buffer
	if err := restoreStream(&out, &slowReader{b: []byte(in)}, mapping); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "<PII_") || strings.Contains(got, "<CREDENTIAL_") {
		t.Fatalf("placeholder leaked: %s", got)
	}
	if !strings.Contains(got, "lixiaofeng@xinghe.example") || !strings.Contains(got, "sk-abcdefghijklmnop1234") {
		t.Fatalf("values not restored: %s", got)
	}
	if !strings.Contains(got, "[DONE]") {
		t.Fatalf("stream tail lost: %s", got)
	}
}

func TestRestoreStreamEmptyMappingPassesThrough(t *testing.T) {
	in := "data: {\"content\":\"plain text, 包含中文\"}\n\ndata: [DONE]\n\n"
	var out bytes.Buffer
	if err := restoreStream(&out, &slowReader{b: []byte(in)}, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Fatalf("empty mapping must pass through byte-identical.\n got: %s\nwant: %s", out.String(), in)
	}
}
