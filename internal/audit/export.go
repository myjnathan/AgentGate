// Audit export (F4).
//
// GET /api/audit/export streams the hash-chained audit log as NDJSON
// (one JSON object per line, oldest first) for a time range, so an
// auditor can pull exactly the window they need into jq, BigQuery,
// or any SIEM that speaks NDJSON — the format Cloudflare Logpull and
// LiteLLM's S3 audit export both standardise on. The response carries
// the chain verification result in headers (X-Audit-Chain-Valid,
// X-Audit-Event-Count, X-Audit-First-Seq, X-Audit-Last-Seq,
// X-Audit-Head-Hash), so the exported package is self-describing:
// the count and head hash let the recipient confirm nothing was
// dropped or added after export. Events never contain plaintext
// secrets or detected sensitive values, so the export is safe to
// hand to an external auditor.
package audit

import (
	"encoding/json"
	"io"
	"time"
)

// ExportFilter bounds an export. Since/Until form a half-open window
// [since, until); zero values mean unbounded. Kind/Actor filter like
// Log.Query; empty means no filter.
type ExportFilter struct {
	Since time.Time
	Until time.Time
	Kind  string
	Actor string
}

// ExportSummary describes what an export contained, so the caller can
// prove the package is complete: Count events, sequence range, the
// hash of the last exported event (HeadHash), and whether the whole
// log's hash chain verified at export time.
type ExportSummary struct {
	Count      int64
	FirstSeq   int64
	LastSeq    int64
	HeadHash   string
	ChainValid bool
}

func (f ExportFilter) matches(ev Event) bool {
	if !f.Since.IsZero() && ev.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !ev.Time.Before(f.Until) {
		return false
	}
	if f.Kind != "" && ev.Kind != f.Kind {
		return false
	}
	if f.Actor != "" && ev.Actor != f.Actor {
		return false
	}
	return true
}

// Export writes the matching events as NDJSON (oldest first) and
// returns a summary with the chain verification result. The summary
// is computed over the full log, not just the exported slice, so a
// recipient can tell whether the source log was intact when the
// export was taken.
func (l *Log) Export(w io.Writer, f ExportFilter) (ExportSummary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sum := ExportSummary{ChainValid: verifyEvents(l.events)}
	enc := json.NewEncoder(w)
	for _, ev := range l.events {
		if !f.matches(ev) {
			continue
		}
		if err := enc.Encode(ev); err != nil {
			return sum, err
		}
		if sum.Count == 0 {
			sum.FirstSeq = ev.Seq
		}
		sum.LastSeq = ev.Seq
		sum.HeadHash = ev.Hash
		sum.Count++
	}
	return sum, nil
}
