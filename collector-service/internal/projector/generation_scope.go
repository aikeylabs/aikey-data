package projector

import (
	"log/slog"
	"strings"
)

// 2026-07-15 非生成流量不进用量审计与统计 (update/20260715-非生成流量不进用量审计与统计.md).
//
// Problem: gateway clients (OpenClaw etc.) poll providers every few minutes
// with non-generation calls (GET /v1/models health checks, model-list
// fetches). The proxy records a usage event for EVERY forwarded request, so
// these zero-token rows flooded the usage-audit page (97% of one org's rows).
//
// Design: the proxy reports the request path as a FACT (wire field
// `request_path`); this table + rule is the ONE place that turns the fact
// into a verdict. Exhaustive suffix table, not scattered if/else — new
// generation endpoints get added here.
//
// generationPathSuffixes lists the URL-path suffixes of GENERATION endpoints —
// calls that can produce token usage. Matched case-insensitively against the
// inbound path with any trailing slash removed. Suffix (not equality) so
// provider-prefix (/anthropic/v1/messages), app (/apps/<slug>/v1/messages) and
// probe (/probe/<alias>/v1/messages) route shapes all match.
//
// Deliberately NOT listed (non-generation by design):
//   - /v1/messages/count_tokens — token counting, no usage in response
//   - /v1/models, /models/<id>   — listing/liveness, the polluter this fixes
var generationPathSuffixes = []string{
	"/messages",    // Anthropic Messages API (/v1/messages)
	"/completions", // OpenAI-compatible: /v1/chat/completions AND legacy /v1/completions
	"/responses",   // OpenAI Responses API + codex (/backend-api/codex/responses)
}

// isGenerationPath reports whether path ends with a known generation-endpoint
// suffix. Empty path returns false (callers gate on presence first).
func isGenerationPath(path string) bool {
	p := strings.ToLower(strings.TrimRight(path, "/"))
	for _, suffix := range generationPathSuffixes {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// applyGenerationScope is the FINAL scope override in the enrichment chain.
// It only ever narrows UserUsageScope to non_generation; completion_source /
// quality_status / anomaly fields are untouched so ownership forensics survive.
//
// Rules (each guards a failure mode):
//   - request_path absent → unchanged. Older proxies and pre-2026-07 events
//     don't carry the field, and the 2026-07-15 decision was NO backfill.
//   - total_tokens > 0 → ALWAYS generation (stays as classified). Tokens are
//     ground truth that generation happened; this protects real usage from a
//     stale suffix table (a new provider endpoint missing above would
//     otherwise silently vanish from audit + stats). A WARN flags the stale
//     table instead.
//   - total_tokens == 0 and path is not a generation endpoint →
//     non_generation. Applies to errors too: a 401 on GET /v1/models is still
//     probe traffic. A 401 on /v1/messages (generation attempt that failed)
//     stays visible — failed generation is part of the audit trail.
func applyGenerationScope(fact *DWDFact, rec *ODSRecord) {
	if !rec.RequestPath.Valid || rec.RequestPath.String == "" {
		return
	}
	path := rec.RequestPath.String
	totalTokens := rec.TotalTokens.Int64 // zero when NULL — same semantics here
	if totalTokens > 0 {
		if !isGenerationPath(path) {
			// Tokens prove generation but the table doesn't know this path —
			// the suffix table is stale. Loud, not silent (logging 规范:
			// fallback paths must WARN).
			slog.Warn("generation tokens on unlisted path — update generationPathSuffixes",
				"event.name", "projector.scope.unlisted_generation_path",
				"request_path", path,
				"total_tokens", totalTokens,
				"event_id", rec.EventID,
			)
		}
		return
	}
	if !isGenerationPath(path) {
		fact.UserUsageScope = UsageScopeNonGeneration
	}
}
