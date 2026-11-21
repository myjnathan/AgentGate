package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/myjnathan/agentgate/internal/guard"
)

// streamCopy proxies an upstream SSE stream to the client.
//
// When the request was desensitized (mapping non-empty), placeholders
// in the streamed response must be restored to the original values
// before the client sees them (F10, blueprint probe 3). A placeholder
// can be split across SSE chunks — upstream JSON encoders also emit it
// in the escaped form (\u003cPII_1\u003e) — so restoring each chunk in
// isolation would leak half-restored placeholders. Instead the copy
// holds back a tail of guard.MaxPlaceholderLength(mapping) bytes and
// restores the whole buffered remainder on every read, emitting only
// the stable prefix: a placeholder is only ever emitted after it has
// fully arrived and been restored. This is the holdback pattern
// (nr-llm ADR 88 "live streaming redaction"; Sether's safeDistance):
// never emit a byte until enough following bytes have arrived to know
// it is not part of an in-progress placeholder.
//
// When mapping is empty nothing was redacted, so chunks pass straight
// through with no buffering and no added latency.
func (g *Gateway) streamCopy(w http.ResponseWriter, resp *http.Response, mapping map[string]string) int64 {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	emit := func(b []byte) {
		if len(b) == 0 {
			return
		}
		_, _ = w.Write(b)
		if fl != nil {
			fl.Flush()
		}
	}

	buf := make([]byte, 4096)
	var tail []byte // last 8 KiB emitted, for usage extraction
	track := func(b []byte) {
		tail = append(tail, b...)
		if len(tail) > 8192 {
			tail = tail[len(tail)-8192:]
		}
	}

	if len(mapping) == 0 {
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				track(chunk)
				emit(chunk)
			}
			if err != nil {
				break
			}
		}
		return usageFromTail(tail)
	}

	// Buffered restore path. pending holds restored text not yet
	// emitted; it may end in a partial placeholder, which Restore
	// leaves untouched until the remaining bytes arrive and complete
	// it. Re-restoring already-restored text is a no-op: placeholders
	// are consumed on first restore and originals never contain
	// placeholder syntax.
	holdback := guard.MaxPlaceholderLength(mapping)
	var pending []byte
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			restored := guard.Restore(string(pending), mapping)
			if len(restored) > holdback {
				cut := len(restored) - holdback
				// Never split a multi-byte UTF-8 rune at the cut.
				for cut > 0 && cut < len(restored) && restored[cut]&0xC0 == 0x80 {
					cut--
				}
				head := []byte(restored[:cut])
				track(head)
				emit(head)
				pending = append([]byte(nil), restored[cut:]...)
			} else {
				pending = []byte(restored)
			}
		}
		if err != nil {
			break
		}
	}
	if len(pending) > 0 {
		restored := guard.Restore(string(pending), mapping)
		out := []byte(restored)
		track(out)
		emit(out)
	}
	return usageFromTail(tail)
}

// usageFromTail extracts total_tokens from the final SSE chunk, if
// present. Best effort: a stream without usage simply reports 0.
//
// The previous implementation sliced from one byte before "usage" and
// took text up to the first '}' plus one byte, which produced invalid
// JSON (a leading comma and a dangling brace) — streamed calls always
// reported 0 tokens, so TPM accounting and usage stats silently missed
// them. This version parses the usage object with brace matching.
func usageFromTail(tail []byte) int64 {
	s := string(tail)
	i := strings.LastIndex(s, "\"usage\"")
	if i < 0 {
		return 0
	}
	rest := s[i+len(`"usage"`):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return 0
	}
	rest = strings.TrimLeft(rest[colon+1:], " \t\r\n")
	if !strings.HasPrefix(rest, "{") {
		return 0
	}
	depth, end, inStr := 0, -1, false
	for k := 0; k < len(rest); k++ {
		c := rest[k]
		if inStr {
			if c == '\\' {
				k++
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = k
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return 0
	}
	var wrap struct {
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(`{"usage":`+rest[:end+1]+`}`), &wrap); err != nil {
		return 0
	}
	return wrap.Usage.TotalTokens
}
