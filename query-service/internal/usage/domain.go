// Package usage provides usage query types and repository interfaces.
package usage

import (
	"log/slog"
	"time"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// TimelinePoint is a single data point on a usage curve.
//
// CostUSD (2026-06, cost-pricing Stage 3) is the estimated USD cost for
// the bucket — Σ billable_amount over USD-priced DWD rows. Unpriced rows
// (billable_amount NULL) contribute 0, so the curve never over-counts.
// Always present (no omitempty) — same convention as the token fields, so
// the FE reads it as a plain number without undefined handling.
type TimelinePoint struct {
	Date         string  `json:"date"` // YYYY-MM-DD
	TotalTokens  int64   `json:"total_tokens"`
	RequestCount int64   `json:"request_count"`
	CostUSD      float64 `json:"cost_usd"`
}

// HourlyPoint is an intra-day usage bucket — one slot per hour for a
// given calendar date. Used by the overview "Today used" card to
// render a 24-bar intra-day distribution.
//
// Timezone: post v1.0.3-alpha / bugfix 20260424, the `hour` field is
// 0..23 in the **caller's local timezone** (per the `?tz=<IANA>`
// query param; defaults to UTC when absent). Clients should render
// `hour` verbatim — no further local conversion on the client side
// (double-converting was an early-round bug; see overview's prior
// chart that mapped UTC hour 4 onto the user's local hour slot,
// producing a 4 AM peak for a local-noon request).
type HourlyPoint struct {
	Hour         int   `json:"hour"` // 0..23 in the caller's local tz
	TotalTokens  int64 `json:"total_tokens"`
	RequestCount int64 `json:"request_count"`
}

// ProtocolTimelinePoint adds provider dimension to a timeline point.
// JSON field is "protocol_type" for backward compatibility, but the value
// is provider_code (e.g. "kimi_code", "moonshot", "anthropic") not the wire protocol.
type ProtocolTimelinePoint struct {
	Date         string `json:"date"`
	ProtocolType string `json:"protocol_type"` // actually provider_code
	TotalTokens  int64  `json:"total_tokens"`
	RequestCount int64  `json:"request_count"`
}

// ProtocolHourlyPoint is the intra-day counterpart of
// ProtocolTimelinePoint — one bucket per (hour, provider) within a
// single local-tz calendar day. Added 2026-05-28 to support the "1D"
// range option on /user/usage-ledger: when range=1D the FE stacks
// hourly per-provider bars instead of daily ones.
//
// Hour is 0..23 in the caller's local timezone (matches HourlyPoint).
// ProtocolType field stays for wire compat with the daily variant —
// value is the canonical provider_code, same as ProtocolTimelinePoint.
type ProtocolHourlyPoint struct {
	Hour         int    `json:"hour"`
	ProtocolType string `json:"protocol_type"`
	TotalTokens  int64  `json:"total_tokens"`
	RequestCount int64  `json:"request_count"`
}

// ProtocolTotal is a single slice of a provider pie chart.
//
// Cost fields (2026-06, cost-pricing Stage 3) — same trio added to every
// "total" breakdown:
//   - CostUSD: Σ billable_amount over USD-priced rows (estimated).
//   - PricedRequestCount / UnpricedRequestCount: request_count split by
//     whether the row carried a price. They sum to RequestCount exactly
//     (split by SUM(request_count), not row count), so the FE can render
//     "N of M unpriced ⚠" without the totals drifting.
type ProtocolTotal struct {
	ProtocolType         string  `json:"protocol_type"` // actually provider_code
	TotalTokens          int64   `json:"total_tokens"`
	RequestCount         int64   `json:"request_count"`
	CostUSD              float64 `json:"cost_usd"`
	PricedRequestCount   int64   `json:"priced_request_count"`
	UnpricedRequestCount int64   `json:"unpriced_request_count"`
}

// ModelTotal is a single entry in the per-model usage breakdown for the
// `/user/cost` "Usage by model" chart. Same 4-segment Anthropic cache
// shape as KeyTotal so the FE can reuse the existing stacked-bar idiom
// (uncached / cache_creation / cache_read / output) when rendering.
//
// Model identity is the raw `model` string the provider reported, with
// NULL / empty coalesced to "unknown" at the SQL layer (matches the
// by-protocol "uncategorized" handling). Snapshot-versioned models
// (e.g. `claude-sonnet-4-5-20250929` vs `claude-sonnet-4-6`) are
// surfaced as separate rows — no normalization. Reason: this is the
// minimum-surface design; if snapshot fragmentation becomes a UX
// complaint, add a normalization config table rather than baking
// regex logic into SQL.
type ModelTotal struct {
	Model                    string `json:"model"`
	InputTokens              int64  `json:"input_tokens"`                // 方案 A: PURE (uncached) input; total input = input + cached + creation
	CachedInputTokens        int64  `json:"cached_input_tokens"`         // Anthropic cache_read_input_tokens
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"` // Anthropic cache_creation_input_tokens
	OutputTokens             int64  `json:"output_tokens"`
	TotalTokens              int64  `json:"total_tokens"`
	RequestCount             int64  `json:"request_count"`
	// Cost trio — see ProtocolTotal for semantics.
	CostUSD              float64 `json:"cost_usd"`
	PricedRequestCount   int64   `json:"priced_request_count"`
	UnpricedRequestCount int64   `json:"unpriced_request_count"`
}

// AppTotal is a single row in the "Usage By App" breakdown (added
// 2026-05-25 for /user/usage-ledger's per-app ranking chart). Rows are
// grouped by the tuple (app_slug, provider_code):
//
//   - When `app_slug != ""` the row represents a registered Connected
//     App (first-party like `degrade-detector` or third-party like
//     `claude-mem`). The frontend uses `app_slug` directly as the
//     display label.
//   - When `app_slug == ""` the row represents default `/v1/...` traffic
//     that did NOT go through a registered app (typically direct
//     `aikey use` from a CLI tool — claude / codex / kimi). The
//     frontend maps `provider_code` to a friendly tool name (anthropic
//     → claude, openai → codex, moonshot/kimi_code → kimi) and shows
//     that instead, per the 2026-05-25 user requirement that "direct"
//     CLI calls show as the tool name rather than the provider code.
//
// The reason we don't pre-merge "direct" rows into one bucket at SQL
// level: keeping the (app_slug, provider) tuple intact lets the
// frontend group by tool name AND still distinguish which provider
// each tool family hit (e.g. kimi(moonshot) vs kimi(kimi-code) stay
// as separate rows, matching the existing ProviderMultiSelect chip
// convention).
type AppTotal struct {
	// AppSlug is the registered app's slug, or "" when the row is the
	// "direct" fallback (no app context — direct /v1/... call). Coalesced
	// at SQL level so the client never sees NULL.
	AppSlug string `json:"app_slug"`
	// ProviderCode is the canonical short form (anthropic / openai /
	// moonshot / kimi_code / ...). Frontend uses this for the tool-name
	// mapping on direct rows and also to render provider chips on app rows.
	ProviderCode string `json:"provider_code"`
	TotalTokens  int64  `json:"total_tokens"`
	RequestCount int64  `json:"request_count"`
	// Cost trio — see ProtocolTotal for semantics.
	CostUSD              float64 `json:"cost_usd"`
	PricedRequestCount   int64   `json:"priced_request_count"`
	UnpricedRequestCount int64   `json:"unpriced_request_count"`
}

// SessionTotal is a single entry in the per-session usage breakdown
// powering the Performance page's "Top N sessions" chart. Grouped by
// the session_id column on usage_fact_dwd (proxy attaches via the
// sessionid extractor — Claude Code header, Kimi prompt_cache_key,
// OpenAI conversation_id, etc.).
//
// SampleLabel / SampleVKID / SampleIdentity / SampleAppSlug are
// "representative" fields for each session bucket — picked via MAX/
// MIN aggregates from the rows that contributed. They give the
// frontend enough context to render a useful row label (e.g.
// "claude-cli · Claude Code · 234K") without forcing a per-session
// JOIN to other tables.
//
// "No session" bucket: rows whose session_id is NULL / empty (clients
// that don't carry a session header — curl, generic SDKs, legacy
// data) coalesce into SessionID="". Frontend renders this bucket as
// "(no session)" or similar.
type SessionTotal struct {
	SessionID                string `json:"session_id"`
	SampleVirtualKeyID       string `json:"sample_virtual_key_id,omitempty"`
	SampleAlias              string `json:"sample_alias,omitempty"`    // representative virtual_key_alias
	SampleIdentity           string `json:"sample_identity,omitempty"` // representative OAuth identity (email)
	SampleAppSlug            string `json:"sample_app_slug,omitempty"` // representative app_slug
	InputTokens              int64  `json:"input_tokens"`
	CachedInputTokens        int64  `json:"cached_input_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	TotalTokens              int64  `json:"total_tokens"`
	RequestCount             int64  `json:"request_count"`
}

// KeyTotal is a single entry in the per-key usage breakdown.
//
// Priority for display labels on the client (see web/src/pages/user/
// usage-ledger): `Alias` (personal/team BYOK) → `Identity` (OAuth email)
// → stripped `VirtualKeyID`. Identity was added 2026-04-22 so OAuth
// sessions stop surfacing as raw `session_<hex>` in the "Usage by Key"
// chart.
//
// AppSlug subtitle (2026-05-26): the OAuth direct path now carries a
// UA-derived `app_slug` (e.g. "claude-code", "cursor", "unknown-app").
// Combined with the SQL aggregating by `(identity, app_slug)`, the FE
// renders the slug as a small subtitle under the email — this is what
// disambiguates multiple session rows that previously all collapsed to
// the same email label. See:
//
//	workflow/CI/requirements/2026-05-26-usage-by-key-app-attribution.md
type KeyTotal struct {
	VirtualKeyID             string `json:"virtual_key_id"`
	Alias                    string `json:"alias,omitempty"`             // human-readable key alias (personal/team BYOK)
	Identity                 string `json:"identity,omitempty"`          // email / display_identity (OAuth sessions)
	AppSlug                  string `json:"app_slug,omitempty"`          // UA-derived (OAuth) or registered (Connected App) client app slug
	InputTokens              int64  `json:"input_tokens"`                // 方案 A: PURE (uncached) input; total input = input + cache_read + cache_creation
	CachedInputTokens        int64  `json:"cached_input_tokens"`         // = Anthropic cache_read_input_tokens (legacy column name)
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"` // Anthropic cache_creation_input_tokens
	OutputTokens             int64  `json:"output_tokens"`
	TotalTokens              int64  `json:"total_tokens"`
	RequestCount             int64  `json:"request_count"`
	// Cost trio — see ProtocolTotal for semantics.
	CostUSD              float64 `json:"cost_usd"`
	PricedRequestCount   int64   `json:"priced_request_count"`
	UnpricedRequestCount int64   `json:"unpriced_request_count"`
}

// RecentRequest is a single raw usage event surfaced to the Overview
// "Recent Requests" card. Sourced directly from `usage_event_ods` (not
// the DWD layer) so canary probe rows can be filtered out by the SQL
// `route_source != 'canary'` clause — DWD aggregates strip the
// route_source dimension. Each entry shows the user a recent KEY /
// OAuth-backed forward through aikey-proxy.
//
// Field choice rationale: smallest set that lets the UI render a
// useful row — when, which provider/model, how big, success/failure,
// and which key. We deliberately omit the long ID columns (binding,
// credential, trace) — they bloat the JSON and the UI doesn't show
// them. If a future feature needs deeper detail, link to a separate
// `/v1/usage/personal/request/:id` rather than enlarging this list
// payload.
type RecentRequest struct {
	RequestID      string `json:"request_id"`
	EventTimeMs    int64  `json:"event_time_ms"`
	ProviderCode   string `json:"provider_code"`
	Model          string `json:"model"`
	TotalTokens    int64  `json:"total_tokens"`
	HTTPStatusCode int    `json:"http_status_code"`
	VirtualKeyID   string `json:"virtual_key_id"`
	RequestStatus  string `json:"request_status"` // "success" | "error" | ...
}

// UserRanking is a single entry in the per-user ranking.
type UserRanking struct {
	AccountID    string `json:"account_id"`
	SeatID       string `json:"seat_id"`
	TotalTokens  int64  `json:"total_tokens"`
	RequestCount int64  `json:"request_count"`
}

// QueryParams holds common query filters.
// Personal queries accept either SeatID or AccountID (fallback for personal/BYOK keys).
type QueryParams struct {
	OrgID     string
	SeatID    string
	AccountID string    // used when SeatID is empty (personal key users without org seat)
	StartDate time.Time // inclusive; interpreted in the user's local TZ
	EndDate   time.Time // inclusive; interpreted in the user's local TZ
	Limit     int       // for ranking, default 50

	// AppSlug, when non-empty, narrows the result to events tagged
	// with this Connected App. Powers the Web Apps Detail page's
	// per-app usage cards. Empty = no filter (whole-vault rollup,
	// existing behaviour for /user/cost page).
	//
	// Only `PersonalTimeline` and `PersonalByModelTotal` consume this
	// filter today — the Apps Detail page only needs trend + by-model.
	// Other endpoints (hourly / by-protocol / by-key / recent) ignore
	// the field; extending them is a non-goal of Phase 4 Stage B.
	AppSlug string

	// SessionID, when non-empty, narrows the result to events tagged
	// with this conversation session. Powers the Performance page's
	// drill-down — clicking a row in the Top N sessions chart filters
	// the by-key + by-model charts to just that session. Empty = no
	// filter (default Performance view shows all sessions for the day).
	//
	// Consumed by PersonalByKeyTotal + PersonalByModelTotal. The new
	// PersonalBySessionTotal endpoint deliberately IGNORES this filter
	// — selecting a session shouldn't shrink the session ranking to
	// one row (see design doc §5.3 for the orthogonality rationale).
	SessionID string

	// TZ is the IANA name (e.g. "Asia/Shanghai") of the caller's
	// local time zone. Empty = UTC. Used to bucket per-day / per-hour
	// aggregates so a user in +08:00 sees their 12:00 request at
	// hour 12, not UTC hour 4. See bugfix 20260424 tz-local round.
	TZ string

	// TZLocation is the parsed *time.Location for TZ. Populated by
	// Defaults(); repo code uses this to compute local day boundaries.
	TZLocation *time.Location

	// TZOffsetMs is the current offset (UTC→local) in milliseconds at
	// the query window start. Used by the SQLite query path which
	// cannot load IANA databases. For Postgres repo code ignores this
	// and uses the IANA name directly via AT TIME ZONE.
	TZOffsetMs int64
}

// Defaults fills in zero-value defaults.
func (q *QueryParams) Defaults() {
	if q.TZ == "" {
		q.TZ = "UTC"
	}
	if q.TZLocation == nil {
		loc, err := time.LoadLocation(q.TZ)
		if err != nil {
			// Don't silently drop bad input — a typo like
			// "?tz=Asia/Shangai" (missing 'h') would otherwise render
			// a correct-looking UTC chart, hiding the client's bug.
			slog.Warn("query-service: unknown IANA TZ, falling back to UTC",
				"tz", q.TZ, "error", err)
			loc = time.UTC
			q.TZ = "UTC"
		}
		q.TZLocation = loc
	}
	if q.TZOffsetMs == 0 && q.TZ != "UTC" {
		// Offset at the **query window's start**, not "now". For a
		// January query run in April from America/New_York, now() is
		// EDT (-04:00) but January was EST (-05:00) — using now()
		// would mis-bucket the entire historical window by one hour,
		// not just the DST transition day. Computing via StartDate.In(loc)
		// picks the offset that applies to most of the window; the
		// remaining DST-crossing-within-window edge stays bounded to
		// the single transition day. Postgres isn't affected (uses
		// AT TIME ZONE <iana> per-row). See code review finding #2
		// on bugfix 20260424.
		anchor := q.StartDate
		if anchor.IsZero() {
			anchor = time.Now()
		}
		_, offsetSec := anchor.In(q.TZLocation).Zone()
		q.TZOffsetMs = int64(offsetSec) * 1000
	}
	// Re-interpret StartDate / EndDate as local midnight: the parser
	// read YYYY-MM-DD naïve (UTC), but the client meant "my local
	// calendar day". After this step, StartDate/EndDate are the
	// instant of local midnight so SQL range filters on event_time
	// millis compare against the correct UTC window.
	if !q.StartDate.IsZero() {
		q.StartDate = toLocalMidnight(q.StartDate, q.TZLocation)
	}
	if !q.EndDate.IsZero() {
		q.EndDate = toLocalMidnight(q.EndDate, q.TZLocation)
	}
	if q.EndDate.IsZero() {
		// "Today" in the user's local zone.
		now := time.Now().In(q.TZLocation)
		q.EndDate = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, q.TZLocation)
	}
	if q.StartDate.IsZero() {
		q.StartDate = q.EndDate.AddDate(0, 0, -30)
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
}

// toLocalMidnight takes a naïve date (e.g. parsed from "YYYY-MM-DD" as
// UTC midnight) and returns the same calendar day at midnight in loc.
func toLocalMidnight(d time.Time, loc *time.Location) time.Time {
	y, m, day := d.Date()
	return time.Date(y, m, day, 0, 0, 0, 0, loc)
}

// LocalWindowMs returns the half-open UTC-millis range [start, end)
// that covers the caller's [StartDate, EndDate] calendar window —
// `end` is the instant at local midnight of EndDate + 1 day so
// "2026-04-24 through 2026-04-24" is a single 24h (or 23/25h on DST
// boundaries) window. Repo code passes both to event_time range
// filters.
//
// StartDate / EndDate are already local-midnight after Defaults()
// shifts them into TZLocation, so UnixMilli() yields the correct UTC
// instant.
func (q QueryParams) LocalWindowMs() (startMs, endMs aikeytime.Millis) {
	startMs = aikeytime.FromTime(q.StartDate)
	endMs = aikeytime.FromTime(q.EndDate.AddDate(0, 0, 1))
	return
}
