package semreview

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeGuard is a minimal Qwen3Guard-style endpoint for tests: it
// answers OpenAI-compatible chat completions with a fixed
// Safety/Categories plain-text verdict.
type fakeGuard struct {
	*httptest.Server
	answer   string
	status   int
	lastAuth string
	lastBody string
}

func newFakeGuard(t *testing.T, answer string) *fakeGuard {
	t.Helper()
	g := &fakeGuard{answer: answer, status: 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.lastAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		g.lastBody = string(b)
		if g.status != 200 {
			w.WriteHeader(g.status)
			_, _ = w.Write([]byte(`{"error": "upstream exploded with secrets"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": g.answer}},
			},
		})
	}))
	g.Server = srv
	t.Cleanup(srv.Close)
	return g
}
