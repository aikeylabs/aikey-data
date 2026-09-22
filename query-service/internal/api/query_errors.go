package api

import (
	"sync"
	"time"
)

// Query-error counters surfaced on GET /health.
//
// 🔴 WHY (2026-09-22). `MasterUpstreamLatency` failed on every call for two
// months (it read a table that never existed) and the only trace was a
// `query.usage.failed` log line. Nothing an operator polls ever changed. The
// project rule is that a pipeline health signal must be readable from an
// endpoint, not only from logs (principles/health-signal-surface.md), so every
// error respondQueryError reports is also counted here and exposed on /health.
// `status` stays "ok": installers gate on the HTTP 200 of this endpoint
// (workflow/CD/installer/lib/health.sh), and a query failure must not turn into
// a failed install or a restart loop. The block is for humans and alerting.
// bugfix: workflow/CI/bugfix/2026-09-22-query-service-upstream-latency-wrong-table.md
type queryErrorStats struct {
	mu     sync.Mutex
	byCode map[string]int64
	byOp   map[string]int64
	last   *queryErrorLast
}

type queryErrorLast struct {
	Op    string `json:"op"`
	Code  string `json:"code"`
	Error string `json:"error"`
	At    string `json:"at"` // RFC3339 UTC
}

var queryErrors = &queryErrorStats{byCode: map[string]int64{}, byOp: map[string]int64{}}

func (s *queryErrorStats) record(op, code, msg string, now time.Time) {
	if len(msg) > 200 {
		msg = msg[:200]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byCode[code]++
	s.byOp[op]++
	s.last = &queryErrorLast{Op: op, Code: code, Error: msg, At: now.UTC().Format(time.RFC3339)}
}

// snapshot is what /health serialises under "query_errors".
func (s *queryErrorStats) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	byCode := make(map[string]int64, len(s.byCode))
	for k, v := range s.byCode {
		byCode[k] = v
		total += v
	}
	byOp := make(map[string]int64, len(s.byOp))
	for k, v := range s.byOp {
		byOp[k] = v
	}
	out := map[string]any{"total": total, "by_code": byCode, "by_op": byOp}
	if s.last != nil {
		l := *s.last
		out["last"] = l
	}
	return out
}

func (s *queryErrorStats) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byCode, s.byOp, s.last = map[string]int64{}, map[string]int64{}, nil
}
