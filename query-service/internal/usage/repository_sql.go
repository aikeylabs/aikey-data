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
		SELECT %s AS d, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
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
func (r *sqlRepo) PersonalHourlyTimeline(ctx context.Context, p QueryParams) ([]HourlyPoint, error) {
	filter, id := personalFilter(p)
	// Local day window: [localMidnight, localMidnight+24h) converted
	// back to UTC millis for the event_time range filter. p.StartDate
	// is already local-midnight because QueryParams.Defaults() shifted
	// it into p.TZLocation.
	dayStart := aikeytime.FromTime(p.StartDate)
	dayEnd := aikeytime.FromTime(p.StartDate.AddDate(0, 0, 1))

	hourExpr := r.db.HourBucketLocal("event_time", p.TZOffsetMs, p.TZ)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS hour, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND event_time >= ? AND event_time < ?
		GROUP BY hour
		ORDER BY hour`, hourExpr, filter),
		id, r.db.BindMillis(dayStart), r.db.BindMillis(dayEnd))
	if err != nil {
		return nil, fmt.Errorf("personal hourly timeline: %w", err)
	}
	defer rows.Close()
	var result []HourlyPoint
	for rows.Next() {
		var hp HourlyPoint
		if err := rows.Scan(&hp.Hour, &hp.TotalTokens, &hp.RequestCount); err != nil {
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

func (r *sqlRepo) PersonalByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error) {
	filter, id := personalFilter(p)
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
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
//   - app_slug != ''  → traffic that went through /apps/<slug>/v1/...,
//     i.e. a registered Connected App (first- or third-party).
//   - app_slug == ''  → "direct" traffic that hit /v1/... without an
//     app context. The frontend maps the provider_code to a friendly
//     tool name (claude / codex / kimi) for display, per the 2026-05-25
//     "show CLI tool name for direct calls" requirement.
//
// We COALESCE both columns at SQL level so:
//   (a) NULL groupings collapse to '' rather than splitting NULL vs.
//       '' into separate buckets (some old rows have empty strings
//       instead of NULL),
//   (b) the scanner can use plain `string` without sql.NullString.
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
		       COALESCE(SUM(request_count), 0)
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
		if err := rows.Scan(&at.AppSlug, &at.ProviderCode, &at.TotalTokens, &at.RequestCount); err != nil {
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
	filter, id := personalFilter(p)
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
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(d.virtual_key_id, ''),
		       COALESCE(NULLIF(MAX(d.virtual_key_alias), ''), REPLACE(COALESCE(d.virtual_key_id, ''), 'personal:', '')),
		       COALESCE(id.identity, ''),
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
		GROUP BY COALESCE(d.virtual_key_id, ''), id.identity
		ORDER BY SUM(d.total_tokens) DESC`, filter),
		startMsArg, endMsArg, id, startMsArg, endMsArg)
	if err != nil {
		return nil, fmt.Errorf("personal by-key total: %w", err)
	}
	defer rows.Close()

	var result []KeyTotal
	for rows.Next() {
		var kt KeyTotal
		if err := rows.Scan(&kt.VirtualKeyID, &kt.Alias, &kt.Identity, &kt.InputTokens, &kt.CachedInputTokens, &kt.CacheCreationInputTokens, &kt.OutputTokens, &kt.TotalTokens, &kt.RequestCount); err != nil {
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
// Why COALESCE(NULLIF(model,''), 'unknown'): the `model` column can
// be NULL or empty for rows captured before the upstream response was
// parsed (proxy fast-path). Bucketing them into an explicit
// "unknown" group preserves SUM accuracy — silent NULL drops would
// under-report total tokens versus the by-key chart on the same
// page.
func (r *sqlRepo) PersonalByModelTotal(ctx context.Context, p QueryParams) ([]ModelTotal, error) {
	filter, id := personalFilter(p)
	startMs, endMs := p.LocalWindowMs()
	appSlugFrag, appSlugArg := appSlugFilter(p)
	args := []interface{}{id, r.db.BindMillis(startMs), r.db.BindMillis(endMs)}
	if appSlugArg != nil {
		args = append(args, appSlugArg)
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(NULLIF(model, ''), 'unknown') AS model_grp,
		       COALESCE(SUM(input_tokens),0),
		       COALESCE(SUM(cached_input_tokens),0),
		       COALESCE(SUM(cache_creation_input_tokens),0),
		       COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(total_tokens),0),
		       COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND event_time >= ? AND event_time < ?%s
		GROUP BY COALESCE(NULLIF(model, ''), 'unknown')
		ORDER BY SUM(total_tokens) DESC
		LIMIT 20`, filter, appSlugFrag),
		args...)
	if err != nil {
		return nil, fmt.Errorf("personal by-model total: %w", err)
	}
	defer rows.Close()

	var result []ModelTotal
	for rows.Next() {
		var mt ModelTotal
		if err := rows.Scan(&mt.Model, &mt.InputTokens, &mt.CachedInputTokens, &mt.CacheCreationInputTokens, &mt.OutputTokens, &mt.TotalTokens, &mt.RequestCount); err != nil {
			return nil, err
		}
		result = append(result, mt)
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

// --- Master page ---

func (r *sqlRepo) MasterUserRanking(ctx context.Context, p QueryParams) ([]UserRanking, error) {
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, `
		SELECT account_id, seat_id, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
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
		if err := rows.Scan(&ur.AccountID, &ur.SeatID, &ur.TotalTokens, &ur.RequestCount); err != nil {
			return nil, err
		}
		result = append(result, ur)
	}
	return result, rows.Err()
}

func (r *sqlRepo) MasterByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error) {
	startMs, endMs := p.LocalWindowMs()
	rows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
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
		SELECT %s AS d, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
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

// --- scan helpers ---

func scanTimeline(rows *sql.Rows) ([]TimelinePoint, error) {
	var result []TimelinePoint
	for rows.Next() {
		var tp TimelinePoint
		if err := rows.Scan(&tp.Date, &tp.TotalTokens, &tp.RequestCount); err != nil {
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
		if err := rows.Scan(&pt.ProtocolType, &pt.TotalTokens, &pt.RequestCount); err != nil {
			return nil, err
		}
		result = append(result, pt)
	}
	return result, rows.Err()
}
