package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dripReader delivers its bytes in fixed small steps, so a placeholder
// is guaranteed to be split across Read boundaries — including inside
// the escaped \u003c…\u003e form and inside multi-byte UTF-8 runes.
type dripReader struct {
	data []byte
	step int
	pos  int
}

func (d *dripReader) Read(p []byte) (int, error) {
	if d.pos >= len(d.data) {
		return 0, io.EOF
	}
	n := d.step
	if d.pos+n > len(d.data) {
		n = len(d.data) - d.pos
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, d.data[d.pos:d.pos+n])
	d.pos += n
	return n, nil
}

func fakeSSEResponse(t *testing.T, body string, step int) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&dripReader{data: []byte(body), step: step}),
	}
}

func TestStreamCopyRestoresSplitPlaceholder(t *testing.T) {
	mapping := map[string]string{"<PII_1>": "13812345678", "<CUSTOM_1>": "XH-200"}
	// One data line carries the placeholder in its JSON-escaped form
	// (\u003cPII_1\u003e, as Go's json.Marshal emits), the next in its
	// raw form; both are split across reads. Chinese text around them
	// exercises the UTF-8 cut back-off.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"您的手机号是\\u003cPII_1\\u003e，请确认\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"项目 <CUSTOM_1> 进展正常\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":42}}\n\n" +
		"data: [DONE]\n\n"
	for _, step := range []int{1, 2, 3, 7} {
		var g Gateway
		rec := httptest.NewRecorder()
		used := g.streamCopy(rec, fakeSSEResponse(t, body, step), mapping)
		out := rec.Body.String()
		if strings.Contains(out, "PII_1") || strings.Contains(out, "CUSTOM_1") {
			t.Fatalf("step %d: placeholder leaked into stream: %q", step, out)
		}
		if !strings.Contains(out, "13812345678") {
			t.Fatalf("step %d: phone not restored: %q", step, out)
		}
		if !strings.Contains(out, "XH-200") {
			t.Fatalf("step %d: project code not restored: %q", step, out)
		}
		if !strings.Contains(out, "您的手机号是") {
			t.Fatalf("step %d: multi-byte text corrupted: %q", step, out)
		}
		if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
			t.Fatalf("step %d: stream must end with [DONE]: %q", step, out)
		}
		if used.Total != 42 {
			t.Fatalf("step %d: expected usage 42, got %d", step, used.Total)
		}
	}
}

func TestStreamCopyPlaceholderAtEndOfStream(t *testing.T) {
	// A placeholder that only completes in the final bytes must still
	// be restored by the EOF flush, not emitted half-restored.
	mapping := map[string]string{"<PII_1>": "13812345678"}
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"尾号是\\u003cPII_1\\u003e\"}}]}\n\n" +
		"data: [DONE]\n\n"
	var g Gateway
	rec := httptest.NewRecorder()
	g.streamCopy(rec, fakeSSEResponse(t, body, 1), mapping)
	out := rec.Body.String()
	if strings.Contains(out, "PII_1") {
		t.Fatalf("placeholder leaked: %q", out)
	}
	if !strings.Contains(out, "13812345678") {
		t.Fatalf("phone not restored at end of stream: %q", out)
	}
}

func TestStreamCopyPassthroughWithoutMapping(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	var g Gateway
	rec := httptest.NewRecorder()
	used := g.streamCopy(rec, fakeSSEResponse(t, body, 2), nil)
	if rec.Body.String() != body {
		t.Fatalf("passthrough must be byte-identical:\n got %q\nwant %q", rec.Body.String(), body)
	}
	if used.Total != 7 {
		t.Fatalf("expected usage 7, got %d", used.Total)
	}
}

func TestStreamCopyUsageSplit(t *testing.T) {
	// F13: pricing needs the prompt/completion split, not just the
	// total — a stream reporting both must keep them apart.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":12,\"total_tokens\":42}}\n\n" +
		"data: [DONE]\n\n"
	var g Gateway
	rec := httptest.NewRecorder()
	used := g.streamCopy(rec, fakeSSEResponse(t, body, 2), nil)
	if used.Prompt != 30 || used.Completion != 12 || used.Total != 42 {
		t.Fatalf("usage split = %+v, want prompt=30 completion=12 total=42", used)
	}
}

func TestStreamCopyEmptyMappingPassthrough(t *testing.T) {
	// An empty (non-nil) mapping is the same as no redaction.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"x<PII_1>y\"}}]}\n\ndata: [DONE]\n\n"
	var g Gateway
	rec := httptest.NewRecorder()
	g.streamCopy(rec, fakeSSEResponse(t, body, 1), map[string]string{})
	if !strings.Contains(rec.Body.String(), "x<PII_1>y") {
		t.Fatalf("empty mapping must not restore: %q", rec.Body.String())
	}
}

func TestStreamCopyLongerPlaceholderHoldback(t *testing.T) {
	// Two placeholders of different lengths: the holdback must cover
	// the longest (escaped) one, or the long placeholder split across
	// the cut leaks half-restored.
	mapping := map[string]string{
		"<PII_1>":        "13812345678",
		"<CREDENTIAL_1>": "sk-abcdefghij123456",
	}
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"密钥 \\u003cCREDENTIAL_1\\u003e 已轮换，手机 \\u003cPII_1\\u003e\"}}]}\n\n" +
		"data: [DONE]\n\n"
	for _, step := range []int{1, 5} {
		var g Gateway
		rec := httptest.NewRecorder()
		g.streamCopy(rec, fakeSSEResponse(t, body, step), mapping)
		out := rec.Body.String()
		if strings.Contains(out, "CREDENTIAL_1") || strings.Contains(out, "PII_1") {
			t.Fatalf("step %d: placeholder leaked: %q", step, out)
		}
		if !strings.Contains(out, "sk-abcdefghij123456") || !strings.Contains(out, "13812345678") {
			t.Fatalf("step %d: values not restored: %q", step, out)
		}
	}
}
