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
	Hour         int     `json:"hour"` // 0..23 in the caller's local tz
	TotalTokens  int64   `json:"total_tokens"`
	RequestCount int64   `json:"request_count"`
	CostUSD      float64 `json:"cost_usd"` // USD billable summed for the hour
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
//   - PricedRequestCount / UnpricedRequestCount: canonical client requests
//     split by whether the selected attempt carried a price. They sum to
//     RequestCount exactly, so the FE can render "N of M unpriced ⚠" without
//     the totals drifting when one client request has multiple attempts.
// UpstreamStepAround is how often traffic was switched AWAY from an upstream
// and onto the next one, per (provider, reason) (openspec change
// `aliyun-aigw-p0-upstream-fallback`, task 4.5b).
//
// # 🔴 Why the console gets a COUNT and not a countdown
//
// Task 4.5b was written asking for "cooling · 4m12s remaining". That number is
// LIVE state on a developer's machine, and I23 forbids live state from reaching
// the control plane at all — derived numbers may travel, living state may not.
// The two requirements were both accepted and they are incompatible.
//
// A count of past switches is the derived form, it is allowed, and it answers
// what 4.5b actually needed: an administrator seeing the bill move to the backup
// vendor should be able to see WHY without concluding they misconfigured
// something. What it does not do is tell them how long the step-around lasts —
// and that limitation is stated on the page rather than papered over.
type UpstreamStepAround struct {
	// ProviderCode is the upstream that SERVED the request after the switch.
	ProviderCode string `json:"provider_code"`
	// Reason is the frozen error code that caused it. Empty = recorded without
	// one (an older proxy), which is not the same as "no reason".
	Reason string `json:"reason"`
	// Switches is how many requests reached this upstream by switching.
	Switches int64 `json:"switches"`
	// LastAt is the most recent one, unix millis; 0 when unknown.
	LastAt int64 `json:"last_at"`
}

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

// AgentTotal is one row of the "Usage By Agent" breakdown on
// /user/usage-ledger (2026-07-17, requirement 2026-07-17-usage-ledger-by-agent-
// breakdown). One row per seat_id: the calling user's OWN seat plus every seat
// whose parent_seat_id is the caller's — i.e. their Agents (数字员工). This
// realizes the D3 "计费按席位" model as a display dimension (每个 Agent 各算一个
// 席位、归属父席位), WITHOUT new tables/columns — it reads usage_fact_dwd.seat_id
// joined to org_seats for the label + agent/parent metadata.
type AgentTotal struct {
	SeatID string `json:"seat_id"`
	// SeatAlias is org_seats.alias (current); "" when the seat has no org_seats
	// row (legacy tags) or no alias set. Frontend falls back to a short seat_id.
	SeatAlias string `json:"seat_alias"`
	// IsAgent is true when the seat's seat_type != 'human' (a digital employee /
	// Agent). The caller's own (human) seat row has IsAgent=false.
	IsAgent bool `json:"is_agent"`
	// ParentSeatID is org_seats.parent_seat_id — the owner seat. For Agent rows it
	// equals the calling user's seat (that's the authorization scope); "" for the
	// caller's own human row.
	ParentSeatID string `json:"parent_seat_id"`
	TotalTokens  int64  `json:"total_tokens"`
	RequestCount int64  `json:"request_count"`
	// Cost trio — same semantics as AppTotal / ProtocolTotal.
	CostUSD              float64 `json:"cost_usd"`
	PricedRequestCount   int64   `json:"priced_request_count"`
	UnpricedRequestCount int64   `json:"unpriced_request_count"`
	// Optional latest-route observation (requested with include_last_route=true).
	// These fields are point-in-time facts from ODS, not the allocation ledger:
	// a request may have temporarily failed over to a different pool account.
	LastAccountID     string `json:"last_account_id,omitempty"`
	LastOAuthIdentity string `json:"last_oauth_identity,omitempty"`
	LastRequestAtMs   int64  `json:"last_request_at_ms,omitempty"`
	LastRequestStatus string `json:"last_request_status,omitempty"`
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

// UsageDetailRow is one per-request row for the Usage Detail page (last 7 days,
// drill-down). Richer than RecentRequest: full token breakdown + cost so the
// "未计价" (billable nil) rows are visible. Cost/tokens/status come from
// usage_fact_dwd (the curated read model — that's where projection computes
// billable_amount; the raw ODS has it NULL). ErrorCode/ErrorMessage are LEFT
// JOINed from the raw ODS (the only place the upstream error text lives) for the
// click-to-expand failure detail. CQRS read of the local store — personal events
// on control.db, team events on Production.
type UsageDetailRow struct {
	EventTimeMs              int64   `json:"event_time_ms"`
	Model                   string  `json:"model"`
	ProviderCode            string  `json:"provider_code"`
	RequestStatus           string  `json:"request_status"`
	HTTPStatusCode          int     `json:"http_status_code"`
	ErrorCode               string  `json:"error_code"`    // from ODS (JOIN); failure detail
	ErrorMessage            string  `json:"error_message"` // from ODS (JOIN); shown in row expand
	LatencyMs               int64   `json:"latency_ms"`    // finished_at - started_at (ODS); 0 if no timing
	InputTokens             int64   `json:"input_tokens"` // PURE (uncached) — 方案 A
	CachedInputTokens       int64   `json:"cached_input_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	OutputTokens            int64   `json:"output_tokens"`
	TotalTokens             int64   `json:"total_tokens"`
	BillableAmount          *string `json:"billable_amount"` // nil = 未计价
	Currency                string  `json:"currency"`
	EndpointURL             string  `json:"endpoint_url"`
	SessionID               string  `json:"session_id"`
	VirtualKeyID            string  `json:"virtual_key_id"`
	VirtualKeyAlias         string  `json:"virtual_key_alias"`
	AppSlug                 string  `json:"app_slug"`
}

// MasterUsageAuditRow is one usage_fact_dwd row for the enterprise usage-audit
// page (org-scoped, v1.0.1-alpha.4). Carries the full audit column set so a row
// is self-verifiable from the read model: identity (who/which key), time
// (occurred vs usage_date/billing_period), usage+cost (with pricing_snapshot_id
// so a NULL billable_amount is explainable as "unpriced" not "no charge"), and
// integrity (content_hash/source_id/source_seq projected from ODS in
// v1.0.1-alpha.3). The page shows a trimmed subset; the CSV export emits every
// field. SourceSeq is *int64 — NULL for old-proxy events that carry no sequence.
type MasterUsageAuditRow struct {
	EventID                  string  `json:"event_id"`
	EventTimeMs              int64   `json:"event_time_ms"`
	OccurredAtMs             int64   `json:"occurred_at_ms"`
	UsageDate                string  `json:"usage_date"`
	BillingPeriod            string  `json:"billing_period"`
	AccountID                string  `json:"account_id"`
	SeatID                   string  `json:"seat_id"`
	SeatAlias                string  `json:"seat_alias"` // org_seats.alias (current); "" when no seat row
	ProviderCode             string  `json:"provider_code"`
	Model                    string  `json:"model"`
	ProtocolType             string  `json:"protocol_type"`
	RouteSource              string  `json:"route_source"`
	VirtualKeyID             string  `json:"virtual_key_id"`
	VirtualKeyHash           string  `json:"virtual_key_hash"`
	CredentialID             string  `json:"credential_id"`
	// OAuthIdentity: POINT-IN-TIME email of the OAuth/pool account that actually served
	// the request, denormalized onto the event by the proxy (2026-07-01, usage-audit
	// "selected account" display; routing changes over time so a read-time join would
	// misattribute history). "" for api_key routes / older-proxy events — the page falls
	// back to a client-side credential_id join.
	OAuthIdentity            string  `json:"oauth_identity"`
	CredentialFingerprint    string  `json:"credential_fingerprint"`
	RealKeyHash              string  `json:"real_key_hash"`
	BindingID                string  `json:"binding_id"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	CachedInputTokens        int64   `json:"cached_input_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	ReasoningTokens          int64   `json:"reasoning_tokens"`
	TotalTokens              int64   `json:"total_tokens"`
	BillableAmount           *string `json:"billable_amount"`
	Currency                 string  `json:"currency"`
	PricingSnapshotID        string  `json:"pricing_snapshot_id"`
	QualityStatus            string  `json:"quality_status"`
	ValidationCode           string  `json:"validation_code"`
	AnomalyType              string  `json:"anomaly_type"`
	CompletionSource         string  `json:"completion_source"`
	ContentHash              string  `json:"content_hash"`
	SourceID                 string  `json:"source_id"`
	SourceSeq                *int64  `json:"source_seq"`
}

// UserRanking is a single entry in the per-user ranking.
//
// CostUSD (2026-06-09) is Σ billable_amount over USD-priced rows for the seat —
// same estimated-cost semantics as TimelinePoint/ProtocolTotal. UnpricedRequestCount
// lets the FE flag "N 笔未计价 ⚠" so a low cost isn't mistaken for low usage.
type UserRanking struct {
	AccountID            string  `json:"account_id"`
	SeatID               string  `json:"seat_id"`
	TotalTokens          int64   `json:"total_tokens"`
	RequestCount         int64   `json:"request_count"`
	CostUSD              float64 `json:"cost_usd"`
	UnpricedRequestCount int64   `json:"unpriced_request_count"`
}

// QueryParams holds common query filters.
// Personal queries accept either SeatID or AccountID (fallback for personal/BYOK keys).
type QueryParams struct {
	OrgID     string
	SeatID    string
	AccountID string    // used when SeatID is empty (personal key users without org seat)
	// IncludeLastRoute enriches by-agent rows with the latest non-canary ODS
	// account that actually served each seat. Default false preserves the
	// established usage-ledger aggregation response and query cost.
	IncludeLastRoute bool
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

	// Usage-detail page filters (drill-down). Empty/false = no narrowing.
	// Consumed by PersonalUsageDetail; Model/VirtualKeyID/Protocol are shared
	// with the master usage-audit filters below (same column semantics).
	Model         string // narrow to one model
	VirtualKeyID  string // narrow to one virtual key (drill-down by key)
	Protocol      string // narrow to one protocol_type (drill-down by protocol)
	OAuthIdentity string // narrow to one OAuth email (drill-down by identity — spans multiple vks)
	Unpriced      bool   // only rows with NULL billable_amount (the "未计价" filter)

	// Master usage-audit filters (20260729 用量审计页自由筛选). Empty = no
	// narrowing. Consumed by MasterUsageDetail + StreamMasterUsageExport via
	// masterAuditWhere — both entry points share the same WHERE builder so the
	// on-screen rows and the exported CSV can never diverge in scope.
	CredentialID string // narrow to one provider credential (which pool account served)
	ProviderCode string // narrow to one provider (anthropic / openai / ...)
	QualityStatus string // narrow to one quality_status (exact / partial / ...)
	AnomalyType  string // narrow to one anomaly_type
	// Billing is a three-state pricing filter: "" (all) | "priced"
	// (billable_amount IS NOT NULL) | "unpriced" (IS NULL). A string enum, not
	// two bools, so a single field decides the switch. The personal-page
	// Unpriced bool above predates this and stays untouched (frozen surface).
	Billing string
	// Keyword (20260729 查询分页): free-text fuzzy filter — case-insensitive
	// substring across oauth_identity / provider_code / model / quality_status
	// / seat alias. Server-side because the audit page uses TRUE server
	// pagination: a client-side fuzzy pass over one 20-row page would be
	// meaningless. Shared by detail and export via masterAuditWhere.
	Keyword string
	// Offset for server pagination of MasterUsageDetail (LIMIT p.Limit OFFSET
	// p.Offset). Offset-style (not keyset) because the page window is capped
	// at 31 days + partition pruning bounds the scan, and the UI needs
	// numbered pages + a real total.
	Offset int

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

// UpstreamLatency is the response-time distribution an organization actually
// sees from its upstreams (openspec change `aliyun-aigw-p0-upstream-fallback`,
// task 5.7).
//
// # 🔴 What it exists to prevent
//
// The single-attempt wait limit became configurable in P1b. An administrator can
// now set it to five seconds — and a normal long-context completion can take
// forty. The chain would then treat a healthy upstream that is merely SLOW as a
// failure: switch away, cool it down, and step around a working vendor for
// minutes. Nothing errors; the bill just moves and the answers get worse.
//
// # 🔴 We report, we do not decide
//
// A customer may genuinely want aggressive fast-failure. So this is a WARNING
// with the numbers attached, never a block: "5% of your requests took longer
// than 32s in the last 7 days; a 5s limit would treat those as upstream
// failures." 🚫 Refusing the save would substitute our judgement for theirs on a
// trade-off only they can price.
type UpstreamLatency struct {
	// P95Ms is the 95th percentile of observed upstream latency, in
	// milliseconds. 🔴 Zero means "no data", which is NOT "fast" — see Samples.
	P95Ms int64 `json:"p95_ms"`
	// Samples is how many rows the percentile was computed from. 🔴 The console
	// must not warn off a handful of requests: a P95 over nine samples is the
	// second-slowest of nine, and presenting that as a distribution invites an
	// operator to act on noise.
	Samples int64 `json:"samples"`
	// WindowDays is the span the figure covers, so the sentence on screen can
	// say it rather than leaving the reader to assume.
	WindowDays int `json:"window_days"`
}
