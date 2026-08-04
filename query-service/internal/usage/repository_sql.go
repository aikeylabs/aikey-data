package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

// aikeytime.FromTime / aikeytime.Millis are used via shared.DB.BindMillis.
var _ = aikeytime.Millis(0) // keep import even if only used transitively

type sqlRepo struct{ db *shared.DB }

// NewSQLRepository returns a Repository backed by either PostgreSQL
// or SQLite. Dialect differences are abstracted by shared.DB (see
// internal/shared/dbkit.go). The type/constructor were renamed from
// postgresRepo/NewPostgresRepository 2026-04-24 because the file
// name was misleading — the implementation has always been dual-
// dialect via dbkit.
func NewSQLRepository(db *shared.DB) Repository {
	return &sqlRepo{db: db}
}

// personalFilter returns the WHERE clause and parameter for personal queries.
// Priority:
//  1. SeatID → filter by seat_id (team member with assigned seat)
//  2. OrgID = "personal" → filter by org_id (personal edition, no account mapping)
//  3. AccountID → filter by account_id (fallback for BYOK users)
//
// Why org_id="personal": in local-user mode the proxy reports events with
// org_id="personal" but no account_id. Without this clause the query returns
// empty, and the Usage Ledger shows zero.
func personalFilter(p QueryParams) (clause string, id string) {
	if p.SeatID != "" {
		return "seat_id = ?", p.SeatID
	}
	if p.OrgID == "personal" {
		return "org_id = ?", "personal"
	}
	return "account_id = ?", p.AccountID
}

// Usage-scope filters (2026-07-15 非生成流量不进用量审计与统计, closing the
// gap where the 阶段2 design doc specified "默认过滤 user_usage_scope = normal"
// for stats queries but no SQL ever implemented it). Two DELIBERATELY
// different rules — do not merge them:
//
//   - scopeStatsAnd (stats/charts/rankings): normal only. Excludes both
//     non_generation (probe/poll traffic — GET /v1/models health checks that
//     flooded pages with 0-token rows) and excluded/abnormal
//     (ownership-unverifiable rows, per the original 阶段2 design).
//
//   - scopeAuditAnd (audit + per-event detail pages): excludes ONLY
//     non_generation. excluded/abnormal rows are exactly what an auditor
//     needs to see (pending_review anomalies), so audit must NOT use the
//     stats rule.
//
// Both are plain string literals (user_usage_scope is NOT NULL on DWD) meant
// to be appended to a WHERE fragment. Unqualified column name — every
// consumer either queries usage_fact_dwd directly or aliases it in a scope
// where the name is unambiguous.
const (
	scopeStatsAnd = " AND user_usage_scope = 'normal'"
	scopeAuditAnd = " AND user_usage_scope <> 'non_generation'"
)

// appSlugFilter returns an additional WHERE fragment + bind value when
// QueryParams.AppSlug is non-empty, otherwise empty / nil so callers
// can splice it into the WHERE clause without conditional branching:
//
//	clause, args = applyAppSlug(clause, args, p)
//
// Empty string and NULL both indicate "no app context" on the DWD row
// (CLI / virtual key calls), so a non-empty AppSlug also filters those
// out implicitly. Powers Phase 4 Connected Apps Detail page (Stage B).
func appSlugFilter(p QueryParams) (frag string, arg interface{}) {
	if p.AppSlug == "" {
		return "", nil
	}
	return " AND app_slug = ?", p.AppSlug
}

// appendNonNil returns args with extra appended only if extra is non-nil.
// Saves a callsite-side if branch when threading optional filter binds.
func appendNonNil(args []interface{}, extra interface{}) []interface{} {
	if extra == nil {
		return args
	}
	return append(args, extra)
}

// sessionIDFilter mirrors appSlugFilter for the session_id dimension.
// Empty SessionID is "no filter" — returning all rows including those
// with NULL/empty session_id (the "no session" bucket). A non-empty
// SessionID narrows to events whose session_id equals exactly that
// string; COALESCE handles NULL → ” so an explicit SessionID="" can
// be requested via a separate sentinel if ever needed (not used today).
//
// Added 2026-05-26 for Performance page drill-down. Consumers:
// PersonalByKeyTotal and PersonalByModelTotal.
func sessionIDFilter(p QueryParams) (frag string, arg interface{}) {
	if p.SessionID == "" {
		return "", nil
	}
	return " AND COALESCE(session_id, '') = ?", p.SessionID
}

// --- Personal page ---

// PersonalTimeline groups usage by the caller's local calendar day
// (QueryParams.TZ). A user in +08:00 asking for "2026-04-24" sees
// their local 00:00..24:00 window, not UTC 00..24 which would split
// their morning across two rows. See bugfix 20260424 tz-local round.
func (r *sqlRepo) PersonalTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error) {
	filter, id := personalFilter(p)
	filter += scopeStatsAnd
	startMs, endMs := p.LocalWindowMs() // [local start-day, local end-day+1) in UTC millis
	dateExpr := r.db.DateOfLocal("event_time", p.TZOffsetMs, p.TZ)
	appSlugFrag, appSlugArg := appSlugFilter(p)
	args := []interface{}{id, r.db.BindMillis(startMs), r.db.BindMillis(endMs)}
	if appSlugArg != nil {
		args = append(args, appSlugArg)
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS d, COALESCE(SUM(total_tokens),0), COALESCE(SUM(client_request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0)
		FROM usage_reporting_fact
		WHERE %s
		  AND event_time >= ? AND event_time < ?%s
		GROUP BY d
		ORDER BY d`, dateExpr, filter, appSlugFrag),
		args...)
	if err != nil {
		return nil, fmt.Errorf("personal timeline: %w", err)
	}
	defer rows.Close()
	return scanTimeline(rows)
}

// PersonalHourlyTimeline aggregates fact rows into 24 hour buckets
// for the calendar day at p.StartDate, interpreted in the caller's
// local timezone (p.TZ). Dialect differences are hidden behind
// shared.DB.HourBucketLocal — see dbkit.go.
//
// AppSlug filter (2026-05-28): honored when non-empty so Apps Detail
// page's hourly view can scope to a single Connected App's traffic.
// Empty AppSlug keeps the existing "whole vault" behavior intact.
func (r *sqlRepo) PersonalHourlyTimeline(ctx context.Context, p QueryParams) ([]HourlyPoint, error) {
	filter, id := personalFilter(p)
	filter += scopeStatsAnd
	// Local day window: [localMidnight, localMidnight+24h) converted
	// back to UTC millis for the event_time range filter. p.StartDate
	// is already local-midnight because QueryParams.Defaults() shifted
	// it into p.TZLocation.
	dayStart := aikeytime.FromTime(p.StartDate)
	dayEnd := aikeytime.FromTime(p.StartDate.AddDate(0, 0, 1))

	hourExpr := r.db.HourBucketLocal("event_time", p.TZOffsetMs, p.TZ)
	appSlugFrag, appSlugArg := appSlugFilter(p)
	args := []interface{}{id, r.db.BindMillis(dayStart), r.db.BindMillis(dayEnd)}
	args = appendNonNil(args, appSlugArg)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS hour, COALESCE(SUM(total_tokens),0), COALESCE(SUM(client_request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0)
		FROM usage_reporting_fact
		WHERE %s
		  AND event_time >= ? AND event_time < ?%s
		GROUP BY hour
		ORDER BY hour`, hourExpr, filter, appSlugFrag),
		args...)
	if err != nil {
		return nil, fmt.Errorf("personal hourly timeline: %w", err)
	}
	defer rows.Close()
	var result []HourlyPoint
	for rows.Next() {
		var hp HourlyPoint
		if err := rows.Scan(&hp.Hour, &hp.TotalTokens, &hp.RequestCount, &hp.CostUSD); err != nil {
			return nil, err
		}
		result = append(result, hp)
	}
	return result, rows.Err()
}

func (r *sqlRepo) PersonalByProtocolTimeline(ctx context.Context, p QueryParams) ([]ProtocolTimelinePoint, error) {
	filter, id := personalFilter(p)
	filter += scopeStatsAnd
	startMs, endMs := p.LocalWindowMs()
	dateExpr := r.db.DateOfLocal("event_time", p.TZOffsetMs, p.TZ)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS d, COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(client_request_count),0)
		FROM usage_reporting_fact
		WHERE %s
		  AND event_time >= ? AND event_time < ?
		GROUP BY d, COALESCE(provider_code, protocol_type)
		ORDER BY d, COALESCE(provider_code, protocol_type)`, dateExpr, filter),
		id, r.db.BindMillis(startMs), r.db.BindMillis(endMs))
	if err != nil {
		return nil, fmt.Errorf("personal by-protocol timeline: %w", err)
	}
	defer rows.Close()

	var result []ProtocolTimelinePoint
	for rows.Next() {
		var pt ProtocolTimelinePoint
		if err := rows.Scan(&pt.Date, &pt.ProtocolType, &pt.TotalTokens, &pt.RequestCount); err != nil {
			return nil, err
		}
		result = append(result, pt)
	}
	return result, rows.Err()
}

// PersonalByProtocolHourly — intra-day per-protocol stack for the "1D"
// range option on /user/usage-ledger. Mirrors PersonalByProtocolTimeline
// shape but groups by hour-bucket instead of date. The single day comes
// from p.StartDate (interpreted in local tz); date range parameters are
// ignored beyond extracting the day.
func (r *sqlRepo) PersonalByProtocolHourly(ctx context.Context, p QueryParams) ([]ProtocolHourlyPoint, error) {
	filter, id := personalFilter(p)
	filter += scopeStatsAnd
	dayStart := aikeytime.FromTime(p.StartDate)
	dayEnd := aikeytime.FromTime(p.StartDate.AddDate(0, 0, 1))
	hourExpr := r.db.HourBucketLocal("event_time", p.TZOffsetMs, p.TZ)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS hour, COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(client_request_count),0)
		FROM usage_reporting_fact
		WHERE %s
		  AND event_time >= ? AND event_time < ?
		GROUP BY hour, COALESCE(provider_code, protocol_type)
		ORDER BY hour, COALESCE(provider_code, protocol_type)`, hourExpr, filter),
		id, r.db.BindMillis(dayStart), r.db.BindMillis(dayEnd))
	if err != nil {
		return nil, fmt.Errorf("personal by-protocol hourly: %w", err)
	}
	defer rows.Close()
	var result []ProtocolHourlyPoint
	for rows.Next() {
		var pt ProtocolHourlyPoint
		if err := rows.Scan(&pt.Hour, &pt.ProtocolType, &pt.TotalTokens, &pt.RequestCount); err != nil {
			return nil, err
		}
		result = append(result, pt)
	}
	return result, rows.Err()
}

func (r *sqlRepo) PersonalByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error) {
	filter, id := personalFilter(p)
	filter += scopeStatsAnd
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(client_request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NOT NULL THEN client_request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN client_request_count ELSE 0 END),0)
		FROM usage_reporting_fact
		WHERE %s
		  AND event_time >= ? AND event_time < ?
		GROUP BY COALESCE(provider_code, protocol_type)
		ORDER BY SUM(total_tokens) DESC`, filter),
		id, r.db.BindMillis(startMs), r.db.BindMillis(endMs))
	if err != nil {
		return nil, fmt.Errorf("personal by-protocol total: %w", err)
	}
	defer rows.Close()
	return scanProtocolTotal(rows)
}

// PersonalByAppTotal aggregates DWD rows by the (app_slug, provider_code)
// pair so the /user/usage-ledger "Usage By App" chart can rank traffic
// by which Connected App generated it. Two row shapes returned:
//
//   - app_slug != ”  → traffic that went through /apps/<slug>/v1/...,
//     i.e. a registered Connected App (first- or third-party).
//   - app_slug == ”  → "direct" traffic that hit /v1/... without an
//     app context. The frontend maps the provider_code to a friendly
//     tool name (claude / codex / kimi) for display, per the 2026-05-25
//     "show CLI tool name for direct calls" requirement.
//
// We COALESCE both columns at SQL level so:
//
//	(a) NULL groupings collapse to '' rather than splitting NULL vs.
//	    '' into separate buckets (some old rows have empty strings
//	    instead of NULL),
//	(b) the scanner can use plain `string` without sql.NullString.
//
// Why GROUP BY both columns instead of just app_slug: the same app slug
// could in theory talk to multiple upstreams over time, and we want
// each (app, upstream) pair to render as its own row in the chart.
// In current data app:upstream is 1:1 but the grouping is the correct
// minimum surface for forward-compat.
//
// Provider fallback uses COALESCE(provider_code, protocol_type),
// matching the existing `PersonalByProtocolTotal` query — some older
// rows have only `protocol_type` populated (provider_code NULL).
func (r *sqlRepo) PersonalByAppTotal(ctx context.Context, p QueryParams) ([]AppTotal, error) {
	filter, id := personalFilter(p)
	filter += scopeStatsAnd
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(app_slug, ''),
		       COALESCE(provider_code, protocol_type, ''),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(client_request_count), 0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NOT NULL THEN client_request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN client_request_count ELSE 0 END),0)
		FROM usage_reporting_fact
		WHERE %s
		  AND event_time >= ? AND event_time < ?
		GROUP BY COALESCE(app_slug, ''), COALESCE(provider_code, protocol_type, '')
		ORDER BY SUM(total_tokens) DESC`, filter),
		id, r.db.BindMillis(startMs), r.db.BindMillis(endMs))
	if err != nil {
		return nil, fmt.Errorf("personal by-app total: %w", err)
	}
	defer rows.Close()
	var out []AppTotal
	for rows.Next() {
		var at AppTotal
		if err := rows.Scan(&at.AppSlug, &at.ProviderCode, &at.TotalTokens, &at.RequestCount,
			&at.CostUSD, &at.PricedRequestCount, &at.UnpricedRequestCount); err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	return out, rows.Err()
}

type latestAgentRoute struct {
	SeatID        string
	SeatAlias     string
	IsAgent       bool
	ParentSeatID  string
	AccountID     string
	OAuthIdentity string
	RequestAtMs   int64
	RequestStatus string
}

func applyLatestAgentRoute(dst *AgentTotal, src latestAgentRoute) {
	dst.LastAccountID = src.AccountID
	dst.LastOAuthIdentity = src.OAuthIdentity
	dst.LastRequestAtMs = src.RequestAtMs
	dst.LastRequestStatus = src.RequestStatus
}

// latestAgentRoutes returns at most one ODS row for the owner seat and each
// Agent it owns. The handler enables this only for Control's server-resolved
// owner scope; this repository still applies the same parent-seat predicate as
// PersonalByAgentTotal. ROW_NUMBER is supported by both shipped SQLite and
// PostgreSQL versions and avoids an unbounded scan/merge in Go.
func (r *sqlRepo) latestAgentRoutes(ctx context.Context, parentSeatID string) ([]latestAgentRoute, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		WITH scoped_seats AS (
			SELECT seat_id, org_id, COALESCE(alias, '') AS seat_alias,
			       CASE WHEN COALESCE(seat_type, 'human') <> 'human' THEN 1 ELSE 0 END AS is_agent,
			       COALESCE(parent_seat_id, '') AS parent_seat_id
			  FROM org_seats
			 WHERE seat_id = ? OR parent_seat_id = ?
		)
		SELECT seat_id, seat_alias, is_agent, parent_seat_id,
		       account_id, oauth_identity, event_time_ms, request_status
		  FROM (
			SELECT o.seat_id,
			       s.seat_alias,
			       s.is_agent,
			       s.parent_seat_id,
			       COALESCE(o.account_id, '') AS account_id,
			       COALESCE(o.oauth_identity, '') AS oauth_identity,
			       %s AS event_time_ms,
			       COALESCE(o.request_status, '') AS request_status,
			       ROW_NUMBER() OVER (PARTITION BY o.seat_id ORDER BY o.event_time DESC, o.ods_id DESC) AS rn
			  FROM usage_event_ods o
			  JOIN scoped_seats s ON s.seat_id = o.seat_id AND s.org_id = o.org_id
			 WHERE COALESCE(o.account_id, '') <> ''
			   AND COALESCE(o.route_source, '') <> 'canary'
		  ) ranked
		 WHERE rn = 1`, r.db.EpochMillis("o.event_time")), parentSeatID, parentSeatID)
	if err != nil {
		return nil, fmt.Errorf("personal latest agent routes: %w", err)
	}
	defer rows.Close()
	out := []latestAgentRoute{}
	for rows.Next() {
		var route latestAgentRoute
		var isAgent int
		if err := rows.Scan(
			&route.SeatID, &route.SeatAlias, &isAgent, &route.ParentSeatID,
			&route.AccountID, &route.OAuthIdentity, &route.RequestAtMs, &route.RequestStatus,
		); err != nil {
			return nil, err
		}
		route.IsAgent = isAgent == 1
		out = append(out, route)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// PersonalByAgentTotal — usage grouped by seat_id for the caller's own seat +
// its Agent seats (org_seats.parent_seat_id = caller). See AgentTotal /
// repository.go for the contract + authorization rationale.
//
// Why seat-scoped (not personalFilter): the "by agent" breakdown must span
// MULTIPLE seat_ids (caller + children), so it cannot use the single-seat
// personalFilter. Scope is enforced in the WHERE by seat_id: seat_ids are
// UUIDs (globally unique), so `d.seat_id = ? OR s.parent_seat_id = ?` returns
// exactly the caller's own usage + their agents' usage and nothing else. The
// LEFT JOIN keeps the caller's own row even when it lacks an org_seats entry
// (matched via d.seat_id = ?), while agent rows are matched via s.parent_seat_id.
func (r *sqlRepo) PersonalByAgentTotal(ctx context.Context, p QueryParams) ([]AgentTotal, error) {
	if p.SeatID == "" {
		// No seat → no agents (personal / BYOK users). Empty, not error.
		return []AgentTotal{}, nil
	}
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, `
		SELECT d.seat_id,
		       COALESCE(s.alias, ''),
		       CASE WHEN COALESCE(s.seat_type, 'human') <> 'human' THEN 1 ELSE 0 END,
		       COALESCE(s.parent_seat_id, ''),
		       COALESCE(SUM(d.total_tokens), 0),
		       COALESCE(SUM(d.client_request_count), 0),
		       COALESCE(SUM(CASE WHEN d.currency='USD' THEN d.billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN d.billable_amount IS NOT NULL THEN d.client_request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN d.billable_amount IS NULL THEN d.client_request_count ELSE 0 END),0)
		FROM usage_reporting_fact d
		LEFT JOIN org_seats s ON s.seat_id = d.seat_id AND s.org_id = d.org_id
		WHERE (d.seat_id = ? OR s.parent_seat_id = ?)
		  AND d.user_usage_scope = 'normal'
		  AND d.event_time >= ? AND d.event_time < ?
		GROUP BY d.seat_id, s.alias, s.seat_type, s.parent_seat_id
		ORDER BY SUM(d.total_tokens) DESC`,
		p.SeatID, p.SeatID, r.db.BindMillis(startMs), r.db.BindMillis(endMs))
	if err != nil {
		return nil, fmt.Errorf("personal by-agent total: %w", err)
	}
	defer rows.Close()
	var out []AgentTotal
	for rows.Next() {
		var at AgentTotal
		var isAgent int
		if err := rows.Scan(&at.SeatID, &at.SeatAlias, &isAgent, &at.ParentSeatID,
			&at.TotalTokens, &at.RequestCount,
			&at.CostUSD, &at.PricedRequestCount, &at.UnpricedRequestCount); err != nil {
			return nil, err
		}
		at.IsAgent = isAgent == 1
		out = append(out, at)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if !p.IncludeLastRoute {
		return out, nil
	}
	latest, err := r.latestAgentRoutes(ctx, p.SeatID)
	if err != nil {
		return nil, err
	}
	bySeat := make(map[string]int, len(out))
	for i := range out {
		bySeat[out[i].SeatID] = i
	}
	for _, route := range latest {
		if i, ok := bySeat[route.SeatID]; ok {
			applyLatestAgentRoute(&out[i], route)
			continue
		}
		// The latest request may predate the requested usage window. Include a
		// zero-usage row only when enrichment was explicitly requested so the
		// Agents page can still show the most recent actual account; established
		// usage-ledger callers (flag absent) keep byte-for-byte row semantics.
		row := AgentTotal{
			SeatID: route.SeatID, SeatAlias: route.SeatAlias, IsAgent: route.IsAgent,
			ParentSeatID: route.ParentSeatID,
		}
		applyLatestAgentRoute(&row, route)
		out = append(out, row)
	}
	return out, nil
}

func (r *sqlRepo) PersonalByKeyTotal(ctx context.Context, p QueryParams) ([]KeyTotal, error) {
	// Identity enrichment (2026-04-22, F2 of the usage-ledger label fix):
	//
	// DWD doesn't carry `oauth_identity` — it dropped during projection —
	// but ODS does. For OAuth sessions the DWD `virtual_key_alias` column
	// is also empty, so without enrichment the UI rendered raw
	// `session_<hex>` strings in the "Usage by Key" chart.
	//
	// Strategy: LEFT-JOIN a pre-aggregated sub-select that pulls
	// oauth_identity per virtual_key_id from ODS within the same date
	// window. Runs once per outer row group (small N — a single user
	// rarely has >10 distinct virtual_key_ids in a window), so cost is
	// negligible in practice. Proper long-term fix: propagate
	// oauth_identity through the projector into DWD — tracked as a
	// follow-up (avoid here because it needs a migration).
	//
	// Aggregation by (identity-or-vk, app_slug) (2026-05-26): the FE
	// labels OAuth rows with the email identity, so grouping by raw
	// virtual_key_id leaked one row per OAuth session and made the
	// same email appear N times. The corrected grouping is:
	//
	//   COALESCE(NULLIF(identity,''), virtual_key_id), COALESCE(app_slug,'')
	//
	// OAuth rows (identity non-empty) collapse per (email, app_slug);
	// non-OAuth rows fall back to (vk_id, app_slug) and behave as
	// before. app_slug is the second dim so the FE can show "same email,
	// different clients" as distinct rows — see
	//   workflow/CI/requirements/2026-05-26-usage-by-key-app-attribution.md
	// and
	//   workflow/CI/bugfix/20260526-usage-by-key-duplicate-rows-by-app-attribution.md
	// for the full rationale, including why displaying ≠ aggregating
	// was the underlying drift.
	filter, id := personalFilter(p)
	filter += scopeStatsAnd
	// Optional session_id filter (2026-05-26 Performance drill-down):
	// when set, the outer WHERE narrows to events tagged with this
	// session. Spliced on the DWD alias `d` since session_id is a DWD
	// column populated by projector transfer.
	sessFrag, sessArg := sessionIDFilter(p)
	if sessArg != nil {
		// Bind on `d.session_id` not the bare column to avoid ambiguity
		// with any future ODS-side session_id reference in the JOIN.
		sessFrag = " AND COALESCE(d.session_id, '') = ?"
	}
	// Both the identity-enrichment subquery (on ODS) and the outer
	// aggregation (on DWD) are filtered by the caller's local-tz
	// window. We express this as a single event_time millis range on
	// each side, same instants on both, so sub-join never drops
	// events the outer query includes.
	startMs, endMs := p.LocalWindowMs()
	startMsArg := r.db.BindMillis(startMs)
	endMsArg := r.db.BindMillis(endMs)
	// NULL-safety on d.virtual_key_id (2026-05-23, BR-rc.5 follow-up): a
	// stray DWD row with virtual_key_id IS NULL crashes the whole query
	// because rows.Scan can't bind NULL → string. Other columns already
	// have COALESCE; this one was a leftover. Mirrors the pattern used
	// in PersonalRequestRecent (line ~336) which already wraps the same
	// column in COALESCE. The frontend's deriveKeyLabel() falls back to
	// "unlabeled" for empty IDs so the row stays surfaced rather than
	// being silently dropped — matches CLAUDE.md "失败要显眼" guidance.
	//
	// MIN(virtual_key_id) is the "representative session" returned to
	// the FE for OAuth rows (where N sessions collapse into one); for
	// non-OAuth rows it equals the only vk_id in the group so behavior
	// is unchanged. The FE no longer uses it as the primary row key —
	// the new react `key` is `${identity_or_vk}|${app_slug}`.
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT MIN(COALESCE(d.virtual_key_id, '')),
		       COALESCE(NULLIF(MAX(d.virtual_key_alias), ''), REPLACE(MIN(COALESCE(d.virtual_key_id, '')), 'personal:', '')),
		       COALESCE(MAX(id.identity), ''),
		       COALESCE(d.app_slug, ''),
		       COALESCE(SUM(d.input_tokens),0),
		       COALESCE(SUM(d.cached_input_tokens),0),
		       COALESCE(SUM(d.cache_creation_input_tokens),0),
		       COALESCE(SUM(d.output_tokens),0),
		       COALESCE(SUM(d.total_tokens),0),
		       COALESCE(SUM(d.client_request_count),0),
		       COALESCE(SUM(CASE WHEN d.currency='USD' THEN d.billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN d.billable_amount IS NOT NULL THEN d.client_request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN d.billable_amount IS NULL THEN d.client_request_count ELSE 0 END),0)
		FROM usage_reporting_fact AS d
		LEFT JOIN (
		    SELECT virtual_key_id, MAX(oauth_identity) AS identity
		    FROM usage_event_ods
		    WHERE oauth_identity IS NOT NULL AND oauth_identity != ''
		      AND event_time >= ? AND event_time < ?
		    GROUP BY virtual_key_id
		) AS id ON id.virtual_key_id = d.virtual_key_id
		WHERE %s
		  AND d.event_time >= ? AND d.event_time < ?%s
		GROUP BY COALESCE(NULLIF(id.identity, ''), COALESCE(d.virtual_key_id, '')), COALESCE(d.app_slug, '')
		ORDER BY SUM(d.total_tokens) DESC`, filter, sessFrag),
		appendNonNil([]interface{}{startMsArg, endMsArg, id, startMsArg, endMsArg}, sessArg)...)
	if err != nil {
		return nil, fmt.Errorf("personal by-key total: %w", err)
	}
	defer rows.Close()

	var result []KeyTotal
	for rows.Next() {
		var kt KeyTotal
		if err := rows.Scan(&kt.VirtualKeyID, &kt.Alias, &kt.Identity, &kt.AppSlug, &kt.InputTokens, &kt.CachedInputTokens, &kt.CacheCreationInputTokens, &kt.OutputTokens, &kt.TotalTokens, &kt.RequestCount,
			&kt.CostUSD, &kt.PricedRequestCount, &kt.UnpricedRequestCount); err != nil {
			return nil, err
		}
		result = append(result, kt)
	}
	return result, rows.Err()
}

// PersonalByModelTotal aggregates DWD rows by the provider-reported
// `model` string and returns the top 20 rows sorted by total_tokens
// DESC. Powers the `/user/cost` "Usage by model" chart.
//
// Why "model" raw (no normalization): snapshot-versioned strings
// (`claude-sonnet-4-5-20250929` vs `claude-sonnet-4-6`) are kept as
// separate rows. A normalization layer (regex → "claude-sonnet-4.5")
// would be a configuration-table concern, not SQL-embedded logic, so
// we defer it until snapshot fragmentation becomes a measurable UX
// problem. See `roadmap20260320/技术实现/update/` if introducing
// model-version normalization later.
//
// Why LIMIT 20: per-tenant model count is bounded in practice
// (single user rarely talks to >10 distinct models in a window). 20
// preserves a long tail for power users while keeping the chart
// readable and the FE row count predictable.
//
// Why COALESCE(NULLIF(model,”), 'unknown'): the `model` column can
// be NULL or empty for rows captured before the upstream response was
// parsed (proxy fast-path). Bucketing them into an explicit
// "unknown" group preserves SUM accuracy — silent NULL drops would
// under-report total tokens versus the by-key chart on the same
// page.
func (r *sqlRepo) PersonalByModelTotal(ctx context.Context, p QueryParams) ([]ModelTotal, error) {
	filter, id := personalFilter(p)
	filter += scopeStatsAnd
	startMs, endMs := p.LocalWindowMs()
	appSlugFrag, appSlugArg := appSlugFilter(p)
	sessFrag, sessArg := sessionIDFilter(p)
	args := []interface{}{id, r.db.BindMillis(startMs), r.db.BindMillis(endMs)}
	args = appendNonNil(args, appSlugArg)
	args = appendNonNil(args, sessArg)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(NULLIF(model, ''), 'unknown') AS model_grp,
		       COALESCE(SUM(input_tokens),0),
		       COALESCE(SUM(cached_input_tokens),0),
		       COALESCE(SUM(cache_creation_input_tokens),0),
		       COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(total_tokens),0),
		       COALESCE(SUM(client_request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NOT NULL THEN client_request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN client_request_count ELSE 0 END),0)
		FROM usage_reporting_fact
		WHERE %s
		  AND event_time >= ? AND event_time < ?%s%s
		GROUP BY COALESCE(NULLIF(model, ''), 'unknown')
		ORDER BY SUM(total_tokens) DESC
		LIMIT 20`, filter, appSlugFrag, sessFrag),
		args...)
	if err != nil {
		return nil, fmt.Errorf("personal by-model total: %w", err)
	}
	defer rows.Close()

	var result []ModelTotal
	for rows.Next() {
		var mt ModelTotal
		if err := rows.Scan(&mt.Model, &mt.InputTokens, &mt.CachedInputTokens, &mt.CacheCreationInputTokens, &mt.OutputTokens, &mt.TotalTokens, &mt.RequestCount,
			&mt.CostUSD, &mt.PricedRequestCount, &mt.UnpricedRequestCount); err != nil {
			return nil, err
		}
		result = append(result, mt)
	}
	return result, rows.Err()
}

// PersonalBySessionTotal aggregates DWD rows by session_id, returning
// the top N (default 10) buckets sorted by total_tokens DESC. Powers
// the /user/performance "Top N sessions" chart.
//
// Aggregation: COALESCE(session_id, ”) so NULL and empty group into a
// single "no session" bucket — the frontend renders this with a clear
// label so users see how much traffic lacks the session dimension.
//
// Identity enrichment (mirrors PersonalByKeyTotal): LEFT JOIN to a
// pre-aggregated ODS subquery so OAuth sessions can surface the user's
// email as a representative label. Without this, OAuth rows would only
// have the opaque session_id from the IDE.
//
// QueryParams.SessionID is INTENTIONALLY ignored: selecting a session
// in the UI shouldn't shrink the ranking to one row — see design doc
// §5.3 "Top N session chart self doesn't receive session filter".
func (r *sqlRepo) PersonalBySessionTotal(ctx context.Context, p QueryParams) ([]SessionTotal, error) {
	filter, id := personalFilter(p)
	filter += scopeStatsAnd
	startMs, endMs := p.LocalWindowMs()
	startMsArg := r.db.BindMillis(startMs)
	endMsArg := r.db.BindMillis(endMs)
	limit := p.Limit
	if limit <= 0 {
		limit = 10
	}
	// Group by session_id alone (not session_id+identity) so the
	// "no session" bucket coalesces all clients without a session
	// header into ONE row — users get one clear "(no session)" entry
	// instead of one per OAuth identity. Identity is surfaced via
	// MAX(id.identity) as a sample for the row label.
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(d.session_id, '')                                   AS session_id,
		       MIN(COALESCE(d.virtual_key_id, ''))                          AS sample_vk_id,
		       COALESCE(NULLIF(MAX(d.virtual_key_alias), ''), '')           AS sample_alias,
		       COALESCE(MAX(id.identity), '')                               AS sample_identity,
		       COALESCE(MAX(d.app_slug), '')                                AS sample_app_slug,
		       COALESCE(SUM(d.input_tokens),0),
		       COALESCE(SUM(d.cached_input_tokens),0),
		       COALESCE(SUM(d.cache_creation_input_tokens),0),
		       COALESCE(SUM(d.output_tokens),0),
		       COALESCE(SUM(d.total_tokens),0),
		       COALESCE(SUM(d.client_request_count),0)
		FROM usage_reporting_fact AS d
		LEFT JOIN (
		    SELECT virtual_key_id, MAX(oauth_identity) AS identity
		    FROM usage_event_ods
		    WHERE oauth_identity IS NOT NULL AND oauth_identity != ''
		      AND event_time >= ? AND event_time < ?
		    GROUP BY virtual_key_id
		) AS id ON id.virtual_key_id = d.virtual_key_id
		WHERE %s
		  AND d.event_time >= ? AND d.event_time < ?
		GROUP BY COALESCE(d.session_id, '')
		ORDER BY SUM(d.total_tokens) DESC
		LIMIT ?`, filter),
		startMsArg, endMsArg, id, startMsArg, endMsArg, limit)
	if err != nil {
		return nil, fmt.Errorf("personal by-session total: %w", err)
	}
	defer rows.Close()

	var result []SessionTotal
	for rows.Next() {
		var st SessionTotal
		if err := rows.Scan(
			&st.SessionID, &st.SampleVirtualKeyID, &st.SampleAlias, &st.SampleIdentity, &st.SampleAppSlug,
			&st.InputTokens, &st.CachedInputTokens, &st.CacheCreationInputTokens,
			&st.OutputTokens, &st.TotalTokens, &st.RequestCount,
		); err != nil {
			return nil, err
		}
		result = append(result, st)
	}
	return result, rows.Err()
}

// PersonalRecent returns the most recent N non-canary requests as raw
// usage_event_ods rows. Unlike the other Personal queries, this one
// touches the ODS layer directly (not DWD aggregates) because:
//
//   - Canary probes (route_source='canary') need to be excluded — DWD
//     aggregates already strip route_source as a dimension, so the
//     aggregated tables don't let us discriminate.
//   - "Recent" semantically means raw rows, one per request — not
//     daily/hourly buckets.
//
// Date-window filter is intentionally omitted: Recent always means
// "newest", regardless of where the caller's chart window sits. If
// QueryParams.StartDate / EndDate were applied here, the card would
// render empty on a stale window (e.g. 30-day chart starting 30 days
// ago — recent activity today would be excluded).
func (r *sqlRepo) PersonalRecent(ctx context.Context, p QueryParams) ([]RecentRequest, error) {
	filter, id := personalFilter(p)
	limit := p.Limit
	if limit <= 0 {
		limit = 5
	}
	// NOTE: `request_id`, `provider_code`, `model`, and other text
	// columns can be NULL in the wild (proxy may emit rows before the
	// upstream response is parsed). Wrap all scanned strings/ints in
	// COALESCE so the Go-side Scan never hits "converting NULL to
	// string is unsupported". Empty-string defaults are safer than
	// switching to sql.NullString — the UI doesn't need to
	// distinguish "missing" from "empty" for a recent-list card.
	// event_time MUST be projected through EpochMillis, not selected raw.
	// usage_event_ods.event_time is INTEGER millis on SQLite but TIMESTAMPTZ on
	// PostgreSQL, and RecentRequest.EventTimeMs is an int64 — so a raw select
	// scans fine on Personal/Trial and blows up on Production with
	//   Scan error on column "event_time": converting time.Time to int64
	// returning 500 QUERY_FAILED for the whole "recent requests" card. Same
	// projection as PersonalDetail below (see the note above it). Found
	// 2026-07-26 by workflow/CI/scripts/cross-app-nav-probe.mjs; the bug had been
	// invisible because every dialect-agnostic test runs on SQLite.
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT
			COALESCE(request_id, '') AS request_id,
			%s AS event_time,
			COALESCE(provider_code, '') AS provider_code,
			COALESCE(model, '') AS model,
			COALESCE(total_tokens, 0) AS total_tokens,
			COALESCE(http_status_code, 0) AS http_status_code,
			COALESCE(virtual_key_id, '') AS virtual_key_id,
			COALESCE(request_status, '') AS request_status
		FROM usage_event_ods
		WHERE %s
		  AND route_source != 'canary'
		ORDER BY event_time DESC
		LIMIT ?`, r.db.EpochMillis("event_time"), filter),
		id, limit)
	if err != nil {
		return nil, fmt.Errorf("personal recent: %w", err)
	}
	defer rows.Close()

	var result []RecentRequest
	for rows.Next() {
		var rr RecentRequest
		// NOTE: http_status_code and total_tokens may be NULL for
		// in-flight rows. sql.NullInt64 / NullString would be more
		// careful but the schema declares NOT NULL with zero defaults
		// on these columns, so direct Scan works on real data.
		if err := rows.Scan(
			&rr.RequestID, &rr.EventTimeMs, &rr.ProviderCode, &rr.Model,
			&rr.TotalTokens, &rr.HTTPStatusCode, &rr.VirtualKeyID,
			&rr.RequestStatus,
		); err != nil {
			return nil, err
		}
		result = append(result, rr)
	}
	return result, rows.Err()
}

// PersonalUsageDetail returns per-request rows for the Usage Detail page. Reads
// usage_event_ods (per-event), windowed by [StartDate, EndDate] (the caller sets
// the last-7-days range; a single ?date= collapses start==end), with optional
// drill-down filters. billable_amount stays NULL for unpriced rows (the "未计价"
// filter selects exactly those).
func (r *sqlRepo) PersonalUsageDetail(ctx context.Context, p QueryParams) ([]UsageDetailRow, error) {
	filter, id := personalFilter(p)
	startMs, endMs := p.LocalWindowMs()
	args := []interface{}{id, r.db.BindMillis(startMs), r.db.BindMillis(endMs)}
	// Audit rule, not stats rule: the detail page keeps excluded/abnormal rows
	// visible (anomaly forensics) and hides only probe/poll traffic.
	where := filter + scopeAuditAnd + " AND route_source != 'canary' AND event_time >= ? AND event_time < ?"
	if p.Unpriced {
		where += " AND billable_amount IS NULL"
	}
	if p.Model != "" {
		where += " AND model = ?"
		args = append(args, p.Model)
	}
	if p.VirtualKeyID != "" {
		where += " AND virtual_key_id = ?"
		args = append(args, p.VirtualKeyID)
	}
	if p.SessionID != "" {
		where += " AND session_id = ?"
		args = append(args, p.SessionID)
	}
	if p.AppSlug != "" {
		where += " AND app_slug = ?"
		args = append(args, p.AppSlug)
	}
	if p.Protocol != "" {
		where += " AND protocol_type = ?"
		args = append(args, p.Protocol)
	}
	if p.OAuthIdentity != "" {
		// Drill-down by OAuth email. One identity spans many virtual keys (the
		// oauth session vk + every app run under that login), so filtering by a
		// single vk_id can't represent the by-key card's email row — this does.
		// oauth_identity is projected onto DWD (rc.12) so this stays on the read
		// model (no ODS join).
		where += " AND oauth_identity = ?"
		args = append(args, p.OAuthIdentity)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 500
	}
	args = append(args, limit)
	// Primary source is usage_fact_dwd (the curated read model): billable_amount is
	// computed during ODS→DWD projection, so the cost column + the "未计价"
	// (billable IS NULL) filter only exist on DWD — and reading DWD makes the
	// unpriced rows match usage-ledger's unpriced count exactly (same source). The
	// upstream error_message/error_code live ONLY on the raw ODS, so we LEFT JOIN it
	// (by event_id) for the click-to-expand failure detail. The inner subquery does
	// the DWD filter/window/limit FIRST (unqualified columns → personalFilter needs
	// no alias + no account_id/seat_id ambiguity with ODS), then we join the (≤limit)
	// rows to ODS. event_time is INTEGER millis on SQLite but TIMESTAMPTZ on Postgres
	// → project via EpochMillis so the int64 Scan works on both.
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS event_time_ms, d.model, d.provider_code,
		       d.request_status, d.http_status_code,
		       COALESCE(o.error_code,''), COALESCE(o.error_message,''),
		       d.input_tokens, d.cached_input_tokens,
		       d.cache_creation_input_tokens, d.output_tokens,
		       d.total_tokens, d.billable_amount, d.currency,
		       d.endpoint_url, d.session_id,
		       d.virtual_key_id, d.virtual_key_alias, d.app_slug,
		       COALESCE(%s, 0) AS latency_ms
		FROM (
			SELECT event_id, event_time, COALESCE(model,'') AS model,
			       COALESCE(provider_code,'') AS provider_code,
			       COALESCE(request_status,'') AS request_status,
			       COALESCE(http_status_code,0) AS http_status_code,
			       COALESCE(input_tokens,0) AS input_tokens,
			       COALESCE(cached_input_tokens,0) AS cached_input_tokens,
			       COALESCE(cache_creation_input_tokens,0) AS cache_creation_input_tokens,
			       COALESCE(output_tokens,0) AS output_tokens,
			       COALESCE(total_tokens,0) AS total_tokens,
			       billable_amount, COALESCE(currency,'') AS currency,
			       COALESCE(endpoint_url,'') AS endpoint_url,
			       COALESCE(session_id,'') AS session_id,
			       COALESCE(virtual_key_id,'') AS virtual_key_id,
			       COALESCE(virtual_key_alias,'') AS virtual_key_alias,
			       COALESCE(app_slug,'') AS app_slug
			FROM usage_fact_dwd
			WHERE %s
			ORDER BY event_time DESC
			LIMIT ?
		) d
		LEFT JOIN usage_event_ods o ON o.event_id = d.event_id
		ORDER BY d.event_time DESC`, r.db.EpochMillis("d.event_time"), r.db.LatencyMillis("o.started_at", "o.finished_at"), where), args...)
	if err != nil {
		return nil, fmt.Errorf("personal usage detail: %w", err)
	}
	defer rows.Close()
	var result []UsageDetailRow
	for rows.Next() {
		var d UsageDetailRow
		var billable sql.NullString
		if err := rows.Scan(&d.EventTimeMs, &d.Model, &d.ProviderCode,
			&d.RequestStatus, &d.HTTPStatusCode, &d.ErrorCode, &d.ErrorMessage,
			&d.InputTokens, &d.CachedInputTokens, &d.CacheCreationInputTokens, &d.OutputTokens,
			&d.TotalTokens, &billable, &d.Currency, &d.EndpointURL, &d.SessionID,
			&d.VirtualKeyID, &d.VirtualKeyAlias, &d.AppSlug, &d.LatencyMs); err != nil {
			return nil, err
		}
		if billable.Valid {
			d.BillableAmount = &billable.String
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// --- Master page ---

func (r *sqlRepo) MasterUserRanking(ctx context.Context, p QueryParams) ([]UserRanking, error) {
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, `
		SELECT account_id, seat_id, COALESCE(SUM(total_tokens),0), COALESCE(SUM(client_request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN client_request_count ELSE 0 END),0)
		FROM usage_reporting_fact
		WHERE org_id = ?
		  AND event_time >= ? AND event_time < ?`+scopeStatsAnd+`
		GROUP BY account_id, seat_id
		ORDER BY SUM(total_tokens) DESC
		LIMIT ?`,
		p.OrgID, r.db.BindMillis(startMs), r.db.BindMillis(endMs), p.Limit)
	if err != nil {
		return nil, fmt.Errorf("master user ranking: %w", err)
	}
	defer rows.Close()

	var result []UserRanking
	for rows.Next() {
		var ur UserRanking
		if err := rows.Scan(&ur.AccountID, &ur.SeatID, &ur.TotalTokens, &ur.RequestCount, &ur.CostUSD, &ur.UnpricedRequestCount); err != nil {
			return nil, err
		}
		result = append(result, ur)
	}
	return result, rows.Err()
}

func (r *sqlRepo) MasterByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error) {
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(client_request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NOT NULL THEN client_request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN client_request_count ELSE 0 END),0)
		FROM usage_reporting_fact
		WHERE org_id = ?
		  AND event_time >= ? AND event_time < ?
		  AND billing_scope IN ('org_only','org_and_user')`+scopeAuditAnd+`
		GROUP BY COALESCE(provider_code, protocol_type)
		ORDER BY SUM(total_tokens) DESC`,
		p.OrgID, r.db.BindMillis(startMs), r.db.BindMillis(endMs))
	if err != nil {
		return nil, fmt.Errorf("master by-protocol total: %w", err)
	}
	defer rows.Close()
	return scanProtocolTotal(rows)
}

func (r *sqlRepo) MasterTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error) {
	startMs, endMs := p.LocalWindowMs()
	dateExpr := r.db.DateOfLocal("event_time", p.TZOffsetMs, p.TZ)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS d, COALESCE(SUM(total_tokens),0), COALESCE(SUM(client_request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0)
		FROM usage_reporting_fact
		WHERE org_id = ?
		  AND event_time >= ? AND event_time < ?
		  AND billing_scope IN ('org_only','org_and_user')`+scopeAuditAnd+`
		GROUP BY d
		ORDER BY d`, dateExpr),
		p.OrgID, r.db.BindMillis(startMs), r.db.BindMillis(endMs))
	if err != nil {
		return nil, fmt.Errorf("master timeline: %w", err)
	}
	defer rows.Close()
	return scanTimeline(rows)
}

// masterAuditSelect builds the dialect-correct projection for an audit row:
// event_time/occurred_at as epoch millis, usage_date as YYYY-MM-DD text (PG DATE
// would otherwise scan as time.Time, not string), nullable strings COALESCE'd to
// ”. Column order must match scanMasterAuditRow.
// Columns are qualified with the `d` (usage_fact_dwd) / `s` (org_seats) aliases
// because masterAuditFrom LEFT JOINs org_seats to resolve seat_alias — org_id /
// account_id / seat_id exist in both tables and would otherwise be ambiguous.
func masterAuditSelect(db *shared.DB) string {
	usageDate := "d.usage_date" // SQLite: already TEXT 'YYYY-MM-DD'
	if !db.IsSQLite() {
		usageDate = "to_char(d.usage_date,'YYYY-MM-DD')" // PG: DATE → text
	}
	return fmt.Sprintf(`d.event_id, %s, %s, %s, COALESCE(d.billing_period,''),
		COALESCE(d.account_id,''), COALESCE(d.seat_id,''), COALESCE(s.alias,''), COALESCE(d.provider_code,''), COALESCE(d.model,''),
		COALESCE(d.protocol_type,''), COALESCE(d.route_source,''),
		COALESCE(d.virtual_key_id,''), COALESCE(d.virtual_key_hash,''),
		COALESCE(d.credential_id,''), COALESCE(d.oauth_identity,''), COALESCE(d.credential_fingerprint,''), COALESCE(d.real_key_hash,''), COALESCE(d.binding_id,''),
		d.input_tokens, d.output_tokens, d.cached_input_tokens, d.cache_creation_input_tokens, d.reasoning_tokens, d.total_tokens,
		d.billable_amount, COALESCE(d.currency,''), COALESCE(d.pricing_snapshot_id,''),
		COALESCE(d.quality_status,''), COALESCE(d.validation_code,''), COALESCE(d.anomaly_type,''), COALESCE(d.completion_source,''),
		COALESCE(d.content_hash,''), COALESCE(d.source_id,''), d.source_seq,
		d.fallback_attempt, COALESCE(d.fallback_reason,'')`,
		db.EpochMillis("d.event_time"), db.EpochMillis("d.occurred_at"), usageDate)
}

// masterAuditFrom: usage_fact_dwd LEFT JOIN org_seats to resolve the current
// seat alias for display. seat_id is org_seats' PK so the join is 1:1 (no row
// fan-out, LIMIT stays exact). LEFT so events whose seat_id has no org_seats row
// (e.g. legacy / non-seat tags) still appear, with seat_alias = ”. org_seats is
// a control-plane table living in the same DB as DWD in every edition that can
// reach the master audit endpoints (Trial / Production share one DB); the
// alternative — denormalising seat_alias into DWD — would freeze a stale alias
// at event time and needs a projector control-plane read, so the read-time join
// is both simpler and shows the *current* alias (matching the page).
const masterAuditFrom = `usage_fact_dwd d LEFT JOIN org_seats s ON s.seat_id = d.seat_id AND s.org_id = d.org_id`

func scanMasterAuditRow(rows *sql.Rows) (*MasterUsageAuditRow, error) {
	var a MasterUsageAuditRow
	var billable sql.NullString
	var sourceSeq sql.NullInt64
	// NullInt64, not int64: NULL means "no chain / pre-feature" and must survive
	// to JSON as null. See MasterUsageAuditRow.FallbackAttempt.
	var fallbackAttempt sql.NullInt64
	if err := rows.Scan(&a.EventID, &a.EventTimeMs, &a.OccurredAtMs, &a.UsageDate, &a.BillingPeriod,
		&a.AccountID, &a.SeatID, &a.SeatAlias, &a.ProviderCode, &a.Model, &a.ProtocolType, &a.RouteSource,
		&a.VirtualKeyID, &a.VirtualKeyHash, &a.CredentialID, &a.OAuthIdentity, &a.CredentialFingerprint, &a.RealKeyHash, &a.BindingID,
		&a.InputTokens, &a.OutputTokens, &a.CachedInputTokens, &a.CacheCreationInputTokens, &a.ReasoningTokens, &a.TotalTokens,
		&billable, &a.Currency, &a.PricingSnapshotID,
		&a.QualityStatus, &a.ValidationCode, &a.AnomalyType, &a.CompletionSource,
		&a.ContentHash, &a.SourceID, &sourceSeq,
		&fallbackAttempt, &a.FallbackReason); err != nil {
		return nil, err
	}
	if billable.Valid {
		a.BillableAmount = &billable.String
	}
	if sourceSeq.Valid {
		v := sourceSeq.Int64
		a.SourceSeq = &v
	}
	if fallbackAttempt.Valid {
		v := fallbackAttempt.Int64
		a.FallbackAttempt = &v
	}
	return &a, nil
}

// masterAuditWhere filters by org + usage_date range (inclusive). usage_date is
// the DWD partition key, so this WHERE prunes the scan to the relevant months.
// p.StartDate/EndDate are interpreted as calendar dates (their YYYY-MM-DD part).
// masterAuditFilterColumns is the audit filter dimension registry (20260729
// 用量审计页自由筛选): each row maps one optional QueryParams field to its
// exact-match DWD column. Adding a filter dimension = adding one row here (+
// handler param + FE config row) — no ad-hoc WHERE branches. All clauses run
// AFTER the usage_date partition pruning, so the scan stays bounded by the
// page's ≤31-day window and needs no new index.
var masterAuditFilterColumns = []struct {
	value  func(QueryParams) string
	clause string
}{
	{func(p QueryParams) string { return p.SeatID }, "d.seat_id = ?"},
	// OAuth identity (20260729 follow-up): the audit table's "OAuth Account"
	// column shows oauth_identity (pool-account email denormalized at event
	// time) — filtering by credential alone can't answer "show me this pool
	// account's calls" because one credential serves many identities.
	{func(p QueryParams) string { return p.OAuthIdentity }, "d.oauth_identity = ?"},
	{func(p QueryParams) string { return p.CredentialID }, "d.credential_id = ?"},
	{func(p QueryParams) string { return p.ProviderCode }, "d.provider_code = ?"},
	{func(p QueryParams) string { return p.Model }, "d.model = ?"},
	{func(p QueryParams) string { return p.QualityStatus }, "d.quality_status = ?"},
	{func(p QueryParams) string { return p.VirtualKeyID }, "d.virtual_key_id = ?"},
	{func(p QueryParams) string { return p.Protocol }, "d.protocol_type = ?"},
	{func(p QueryParams) string { return p.AnomalyType }, "d.anomaly_type = ?"},
}

func masterAuditWhere(p QueryParams) (string, []any) {
	// scopeAuditAnd (2026-07-15): probe/poll traffic (non_generation) is not
	// part of the usage audit; excluded/abnormal rows stay — they carry the
	// pending_review anomalies auditors must see. Qualified with d. because
	// this WHERE runs against the org_seats LEFT JOIN.
	where := "d.org_id = ? AND d.usage_date >= ? AND d.usage_date <= ? AND d.user_usage_scope <> 'non_generation'"
	args := []any{p.OrgID, p.StartDate.Format("2006-01-02"), p.EndDate.Format("2006-01-02")}
	for _, f := range masterAuditFilterColumns {
		if v := f.value(p); v != "" {
			where += " AND " + f.clause
			args = append(args, v)
		}
	}
	// Three-state pricing filter (NULL semantics, so not an equality column
	// above). "unpriced" = no pricing snapshot matched, NOT zero cost.
	switch p.Billing {
	case "priced":
		where += " AND d.billable_amount IS NOT NULL"
	case "unpriced":
		where += " AND d.billable_amount IS NULL"
	}
	// Keyword fuzzy filter (20260729 查询分页): case-insensitive substring
	// across the columns the audit table RENDERS. LOWER(..) LIKE works on both
	// dialects. Escape LIKE metacharacters so a literal '%'/'_' in the query
	// matches itself instead of widening the match.
	if p.Keyword != "" {
		kw := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(p.Keyword))
		pat := "%" + kw + "%"
		where += ` AND (LOWER(COALESCE(d.oauth_identity,'')) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(d.provider_code,'')) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(d.model,'')) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(d.quality_status,'')) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(s.alias,'')) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(s.invited_email,'')) LIKE ? ESCAPE '\')`
		args = append(args, pat, pat, pat, pat, pat, pat)
	}
	return where, args
}

// masterAuditSortColumns whitelists the sortable columns (20260801 排序): the
// key the FE sends → the SQL expression. A map lookup, NEVER interpolation of
// caller input, so ORDER BY stays injection-proof. Expressions COALESCE
// nullable columns so both dialects order deterministically (cost: unpriced
// rows coalesce to -1 — below every real amount, so asc leads with unpriced
// and desc ends with them).
var masterAuditSortColumns = map[string]string{
	"time":     "d.event_time",
	"seat":     "COALESCE(s.alias, s.invited_email, d.seat_id)",
	"identity": "COALESCE(d.oauth_identity,'')",
	"provider": "COALESCE(d.provider_code,'')",
	"model":    "COALESCE(d.model,'')",
	"tokens":   "d.total_tokens",
	"cost":     "COALESCE(d.billable_amount, -1)",
	"quality":  "COALESCE(d.quality_status,'')",
	// 🔴 NULL sorts as 0 — BELOW a primary-served 1 — so descending puts the
	// deepest fallbacks first, which is the reason anyone sorts this column.
	//
	// Collapsing NULL and 0 is safe HERE and only here: sorting asks "which rows
	// first", not "what happened". Every rendering path must keep them apart —
	// NULL means the key has no chain and no hop number was ever measured, while
	// 1 means the primary served it. The drawer, the CSV and the table cell all
	// carry that distinction; an ORDER BY has nowhere to put it.
	"failover": "COALESCE(d.fallback_attempt, 0)",
}

// ValidMasterAuditSortKey reports whether the FE-supplied sort key is in the
// whitelist — the handler rejects unknown keys loudly instead of silently
// falling back.
func ValidMasterAuditSortKey(k string) bool {
	_, ok := masterAuditSortColumns[k]
	return ok
}

// masterAuditOrderBy resolves the ORDER BY clause. The event_time/event_id
// tail keeps pagination stable (no row can straddle two pages) even when the
// primary sort expression has ties.
func masterAuditOrderBy(p QueryParams) string {
	expr, ok := masterAuditSortColumns[p.SortBy]
	if !ok {
		return "d.event_time DESC, d.event_id DESC"
	}
	dir := "DESC"
	if p.SortDir == "asc" {
		dir = "ASC"
	}
	return fmt.Sprintf("%s %s, d.event_time DESC, d.event_id DESC", expr, dir)
}

func (r *sqlRepo) MasterUsageDetail(ctx context.Context, p QueryParams) ([]MasterUsageAuditRow, error) {
	where, args := masterAuditWhere(p)
	// True server pagination (20260729 查询分页): LIMIT+OFFSET with a stable
	// ORDER BY. Offset-style is fine here — the window is capped at 31 days
	// and partition-pruned, so deep offsets stay bounded.
	args = append(args, p.Limit, p.Offset)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT ? OFFSET ?`,
		masterAuditSelect(r.db), masterAuditFrom, where, masterAuditOrderBy(p)), args...)
	if err != nil {
		return nil, fmt.Errorf("master usage detail: %w", err)
	}
	defer rows.Close()
	var result []MasterUsageAuditRow
	for rows.Next() {
		a, err := scanMasterAuditRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *a)
	}
	return result, rows.Err()
}

// MasterUsageDetailTotal returns the full match count for the SAME scope as
// MasterUsageDetail (shared masterAuditWhere) — the real total behind the
// paginated window, so the page shows an honest count instead of a truncation
// banner.
func (r *sqlRepo) MasterUsageDetailTotal(ctx context.Context, p QueryParams) (int64, error) {
	where, args := masterAuditWhere(p)
	var total int64
	if err := r.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s WHERE %s`, masterAuditFrom, where), args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("master usage detail total: %w", err)
	}
	return total, nil
}

// masterAuditFacetColumns: the row-derived filter dimensions whose option
// lists the FE can no longer compute client-side once TRUE pagination ships
// (the browser only holds one page). Keys match the FE dimension keys.
var masterAuditFacetColumns = []struct {
	Key    string
	Column string
}{
	{"identity", "d.oauth_identity"},
	{"model", "d.model"},
	{"vk", "d.virtual_key_id"},
	{"protocol", "d.protocol_type"},
	{"anomaly", "d.anomaly_type"},
}

const masterAuditFacetLimit = 200 // bound each facet payload; freeText covers the tail

// facetParamsExcluding clears the facet dimension's OWN filter — the standard
// faceted-search rule: a dimension's option list must show the ALTERNATIVES to
// the current pick under the OTHER conditions, not just the pick itself.
// Without this, applying e.g. an identity token collapsed the identity facet
// to that one value and the chip-click "adjust in place" flow had nothing to
// offer (user report 2026-07-29). Other dimensions keep narrowing normally.
func facetParamsExcluding(p QueryParams, key string) QueryParams {
	switch key {
	case "identity":
		p.OAuthIdentity = ""
	case "model":
		p.Model = ""
	case "vk":
		p.VirtualKeyID = ""
	case "protocol":
		p.Protocol = ""
	case "anomaly":
		p.AnomalyType = ""
	}
	return p
}

// MasterUsageDetailFacets returns distinct values per row-derived dimension.
// Each facet's scope = the full filter set MINUS that dimension's own filter
// (see facetParamsExcluding). Requested only when the filter set changes (not
// per page flip).
func (r *sqlRepo) MasterUsageDetailFacets(ctx context.Context, p QueryParams) (map[string][]string, error) {
	out := make(map[string][]string, len(masterAuditFacetColumns))
	for _, f := range masterAuditFacetColumns {
		where, args := masterAuditWhere(facetParamsExcluding(p, f.Key))
		args = append(args, masterAuditFacetLimit)
		rows, err := r.db.QueryContext(ctx, fmt.Sprintf(
			`SELECT DISTINCT %s FROM %s WHERE %s AND %s IS NOT NULL AND %s <> '' ORDER BY %s LIMIT ?`,
			f.Column, masterAuditFrom, where, f.Column, f.Column, f.Column), args...)
		if err != nil {
			return nil, fmt.Errorf("master usage facet %s: %w", f.Key, err)
		}
		vals := []string{}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			vals = append(vals, v)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		out[f.Key] = vals
	}
	return out, nil
}

func (r *sqlRepo) StreamMasterUsageExport(ctx context.Context, p QueryParams, fn func(*MasterUsageAuditRow) error) error {
	where, args := masterAuditWhere(p)
	// No LIMIT — full range. The driver reads rows incrementally off the wire as
	// fn consumes them, so memory stays O(1) even for a year-long export.
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s ORDER BY d.event_time DESC`,
		masterAuditSelect(r.db), masterAuditFrom, where), args...)
	if err != nil {
		return fmt.Errorf("master usage export: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanMasterAuditRow(rows)
		if err != nil {
			return err
		}
		if err := fn(a); err != nil {
			return err
		}
	}
	return rows.Err()
}

// --- scan helpers ---

func scanTimeline(rows *sql.Rows) ([]TimelinePoint, error) {
	var result []TimelinePoint
	for rows.Next() {
		var tp TimelinePoint
		if err := rows.Scan(&tp.Date, &tp.TotalTokens, &tp.RequestCount, &tp.CostUSD); err != nil {
			return nil, err
		}
		result = append(result, tp)
	}
	return result, rows.Err()
}

func scanProtocolTotal(rows *sql.Rows) ([]ProtocolTotal, error) {
	var result []ProtocolTotal
	for rows.Next() {
		var pt ProtocolTotal
		if err := rows.Scan(&pt.ProtocolType, &pt.TotalTokens, &pt.RequestCount,
			&pt.CostUSD, &pt.PricedRequestCount, &pt.UnpricedRequestCount); err != nil {
			return nil, err
		}
		result = append(result, pt)
	}
	return result, rows.Err()
}

// MasterUpstreamStepArounds answers "which upstreams did we switch to lately, and
// why" (openspec change `aliyun-aigw-p0-upstream-fallback`, task 4.5b).
//
// # 🔴 `fallback_attempt > 1`, and why NULL must not be swept in
//
// A row with NULL was written before the field existed. Treating NULL as 1 would
// silently classify all historical traffic as primary-served — accidentally right
// today and permanently unrecoverable — while treating it as a switch would
// invent switches that never happened. It is neither, so it is excluded, and the
// count is honestly "switches we have a record of".
//
// # 🔴 Reads usage_fact_dwd, not usage_reporting_fact
//
// The reporting view collapses a failover chain to ONE client request (Order
// 11060), which is exactly right for billing and exactly wrong here: this
// question is about upstream attempts, and the view's whole job is to hide them.
// # 🔴 `MAX(event_time)` must be projected to millis BEFORE the COALESCE
//
// `COALESCE(MAX(event_time), 0)` reads fine and is a hard error on Postgres:
// event_time is INTEGER millis on SQLite but TIMESTAMPTZ there, and
// `COALESCE(timestamptz, 0)` cannot be typed —
//
//	pq: COALESCE types timestamp with time zone and integer cannot be matched (42804)
//
// so this endpoint returned 500 on EVERY Postgres deployment, which is every
// production one. Found 2026-07-31 on the first real-PG run of the acceptance
// suite; the unit tests are SQLite-only, where the same SQL is valid and green.
// Same shape as the 0b.7c view defect: one statement, two dialects, and the
// dialect that is exercised in CI is the one that cannot fail.
//
// 🚫 Do not "fix" it by dropping the COALESCE — MAX over an empty group is still
// NULL and the caller scans into int64. Project first, then COALESCE.
func (r *sqlRepo) MasterUpstreamStepArounds(ctx context.Context, p QueryParams) ([]UpstreamStepAround, error) {
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(provider_code, ''), COALESCE(fallback_reason, ''),
		       COUNT(*), COALESCE(MAX(%s), 0)
		FROM usage_fact_dwd
		WHERE org_id = ?
		  AND event_time >= ? AND event_time < ?
		  AND fallback_attempt IS NOT NULL AND fallback_attempt > 1
		GROUP BY COALESCE(provider_code, ''), COALESCE(fallback_reason, '')
		ORDER BY COUNT(*) DESC`, r.db.EpochMillis("event_time")),
		p.OrgID, r.db.BindMillis(startMs), r.db.BindMillis(endMs))
	if err != nil {
		return nil, fmt.Errorf("master upstream step-arounds: %w", err)
	}
	defer rows.Close()
	var out []UpstreamStepAround
	for rows.Next() {
		var s UpstreamStepAround
		if err := rows.Scan(&s.ProviderCode, &s.Reason, &s.Switches, &s.LastAt); err != nil {
			return nil, fmt.Errorf("scan upstream step-around: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MasterUpstreamLatency computes the org's observed upstream response-time P95
// (openspec change `aliyun-aigw-p0-upstream-fallback`, task 5.7).
//
// # 🔴 Two queries, not `percentile_cont`
//
// The aggregate exists on Postgres and not on SQLite, and this repository serves
// both dialects from one code path. Branching would give the two editions
// different arithmetic for a number an administrator is about to set a threshold
// from — and the disagreement would surface as "the warning says something
// different on my laptop", which is the hardest kind of report to act on.
//
// Counting first and then taking one row at an OFFSET is exact, identical on
// both dialects, and bounded: the OFFSET is resolved server-side, so nothing
// proportional to the window is transferred.
//
// # 🔴 Only rows that reached an upstream
//
// A request rejected before dial has no upstream latency; including its zero
// would drag the percentile toward zero and produce the most dangerous possible
// advice — "your upstreams are fast, a short limit is fine".
func (r *sqlRepo) MasterUpstreamLatency(ctx context.Context, p QueryParams) (UpstreamLatency, error) {
	startMs, endMs := p.LocalWindowMs()
	latency := r.db.LatencyMillis("started_at", "finished_at")
	const windowDays = 7

	where := fmt.Sprintf(`
		FROM usage_fact_ods
		WHERE org_id = ?
		  AND event_time >= ? AND event_time < ?
		  AND started_at IS NOT NULL AND finished_at IS NOT NULL
		  AND %s > 0`, latency)
	args := []any{p.OrgID, r.db.BindMillis(startMs), r.db.BindMillis(endMs)}

	var n int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) "+where, args...).Scan(&n); err != nil {
		return UpstreamLatency{}, fmt.Errorf("master upstream latency count: %w", err)
	}
	if n == 0 {
		// 🔴 Zero samples, zero P95 — and the caller must read those together.
		// "P95 = 0" alone says "instant", which is the opposite of "we do not
		// know", and it is the reading that would suppress the warning entirely.
		return UpstreamLatency{Samples: 0, WindowDays: windowDays}, nil
	}

	// Index of the 95th percentile in ascending order, clamped into range. For
	// small n this lands on the slowest sample, which is the honest answer for a
	// distribution that small — and `Samples` travels alongside so the console
	// can decline to draw a conclusion from it.
	idx := (n * 95) / 100
	if idx >= n {
		idx = n - 1
	}

	var p95 int64
	q := fmt.Sprintf("SELECT %s AS lat %s ORDER BY lat ASC LIMIT 1 OFFSET ?", latency, where)
	if err := r.db.QueryRowContext(ctx, q, append(args, idx)...).Scan(&p95); err != nil {
		return UpstreamLatency{}, fmt.Errorf("master upstream latency p95: %w", err)
	}
	return UpstreamLatency{P95Ms: p95, Samples: n, WindowDays: windowDays}, nil
}
