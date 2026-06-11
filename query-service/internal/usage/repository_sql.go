package usage

import (
	"context"
	"database/sql"
	"fmt"

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
	startMs, endMs := p.LocalWindowMs() // [local start-day, local end-day+1) in UTC millis
	dateExpr := r.db.DateOfLocal("event_time", p.TZOffsetMs, p.TZ)
	appSlugFrag, appSlugArg := appSlugFilter(p)
	args := []interface{}{id, r.db.BindMillis(startMs), r.db.BindMillis(endMs)}
	if appSlugArg != nil {
		args = append(args, appSlugArg)
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS d, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0)
		FROM usage_fact_dwd
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
		SELECT %s AS hour, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0)
		FROM usage_fact_dwd
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
	startMs, endMs := p.LocalWindowMs()
	dateExpr := r.db.DateOfLocal("event_time", p.TZOffsetMs, p.TZ)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS d, COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
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
	dayStart := aikeytime.FromTime(p.StartDate)
	dayEnd := aikeytime.FromTime(p.StartDate.AddDate(0, 0, 1))
	hourExpr := r.db.HourBucketLocal("event_time", p.TZOffsetMs, p.TZ)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS hour, COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
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
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NOT NULL THEN request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN request_count ELSE 0 END),0)
		FROM usage_fact_dwd
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
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(app_slug, ''),
		       COALESCE(provider_code, protocol_type, ''),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(request_count), 0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NOT NULL THEN request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN request_count ELSE 0 END),0)
		FROM usage_fact_dwd
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
		       COALESCE(SUM(d.request_count),0),
		       COALESCE(SUM(CASE WHEN d.currency='USD' THEN d.billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN d.billable_amount IS NOT NULL THEN d.request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN d.billable_amount IS NULL THEN d.request_count ELSE 0 END),0)
		FROM usage_fact_dwd AS d
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
		       COALESCE(SUM(request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NOT NULL THEN request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN request_count ELSE 0 END),0)
		FROM usage_fact_dwd
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
		       COALESCE(SUM(d.request_count),0)
		FROM usage_fact_dwd AS d
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
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT
			COALESCE(request_id, '') AS request_id,
			event_time,
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
		LIMIT ?`, filter),
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
	where := filter + " AND route_source != 'canary' AND event_time >= ? AND event_time < ?"
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
		SELECT account_id, seat_id, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN request_count ELSE 0 END),0)
		FROM usage_fact_dwd
		WHERE org_id = ?
		  AND event_time >= ? AND event_time < ?
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
		SELECT COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NOT NULL THEN request_count ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN billable_amount IS NULL THEN request_count ELSE 0 END),0)
		FROM usage_fact_dwd
		WHERE org_id = ?
		  AND event_time >= ? AND event_time < ?
		  AND billing_scope IN ('org_only','org_and_user')
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
		SELECT %s AS d, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0),
		       COALESCE(SUM(CASE WHEN currency='USD' THEN billable_amount ELSE 0 END),0)
		FROM usage_fact_dwd
		WHERE org_id = ?
		  AND event_time >= ? AND event_time < ?
		  AND billing_scope IN ('org_only','org_and_user')
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
// ''. Column order must match scanMasterAuditRow.
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
		COALESCE(d.credential_id,''), COALESCE(d.credential_fingerprint,''), COALESCE(d.real_key_hash,''), COALESCE(d.binding_id,''),
		d.input_tokens, d.output_tokens, d.cached_input_tokens, d.cache_creation_input_tokens, d.reasoning_tokens, d.total_tokens,
		d.billable_amount, COALESCE(d.currency,''), COALESCE(d.pricing_snapshot_id,''),
		COALESCE(d.quality_status,''), COALESCE(d.validation_code,''), COALESCE(d.anomaly_type,''), COALESCE(d.completion_source,''),
		COALESCE(d.content_hash,''), COALESCE(d.source_id,''), d.source_seq`,
		db.EpochMillis("d.event_time"), db.EpochMillis("d.occurred_at"), usageDate)
}

// masterAuditFrom: usage_fact_dwd LEFT JOIN org_seats to resolve the current
// seat alias for display. seat_id is org_seats' PK so the join is 1:1 (no row
// fan-out, LIMIT stays exact). LEFT so events whose seat_id has no org_seats row
// (e.g. legacy / non-seat tags) still appear, with seat_alias = ''. org_seats is
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
	if err := rows.Scan(&a.EventID, &a.EventTimeMs, &a.OccurredAtMs, &a.UsageDate, &a.BillingPeriod,
		&a.AccountID, &a.SeatID, &a.SeatAlias, &a.ProviderCode, &a.Model, &a.ProtocolType, &a.RouteSource,
		&a.VirtualKeyID, &a.VirtualKeyHash, &a.CredentialID, &a.CredentialFingerprint, &a.RealKeyHash, &a.BindingID,
		&a.InputTokens, &a.OutputTokens, &a.CachedInputTokens, &a.CacheCreationInputTokens, &a.ReasoningTokens, &a.TotalTokens,
		&billable, &a.Currency, &a.PricingSnapshotID,
		&a.QualityStatus, &a.ValidationCode, &a.AnomalyType, &a.CompletionSource,
		&a.ContentHash, &a.SourceID, &sourceSeq); err != nil {
		return nil, err
	}
	if billable.Valid {
		a.BillableAmount = &billable.String
	}
	if sourceSeq.Valid {
		v := sourceSeq.Int64
		a.SourceSeq = &v
	}
	return &a, nil
}

// masterAuditWhere filters by org + usage_date range (inclusive). usage_date is
// the DWD partition key, so this WHERE prunes the scan to the relevant months.
// p.StartDate/EndDate are interpreted as calendar dates (their YYYY-MM-DD part).
func masterAuditWhere(p QueryParams) (string, []any) {
	return "d.org_id = ? AND d.usage_date >= ? AND d.usage_date <= ?",
		[]any{p.OrgID, p.StartDate.Format("2006-01-02"), p.EndDate.Format("2006-01-02")}
}

func (r *sqlRepo) MasterUsageDetail(ctx context.Context, p QueryParams) ([]MasterUsageAuditRow, error) {
	where, args := masterAuditWhere(p)
	args = append(args, p.Limit)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s ORDER BY d.event_time DESC LIMIT ?`,
		masterAuditSelect(r.db), masterAuditFrom, where), args...)
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
