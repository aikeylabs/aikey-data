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

// --- Personal page ---

// PersonalTimeline groups usage by the caller's local calendar day
// (QueryParams.TZ). A user in +08:00 asking for "2026-04-24" sees
// their local 00:00..24:00 window, not UTC 00..24 which would split
// their morning across two rows. See bugfix 20260424 tz-local round.
func (r *sqlRepo) PersonalTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error) {
	filter, id := personalFilter(p)
	startMs, endMs := p.LocalWindowMs() // [local start-day, local end-day+1) in UTC millis
	dateExpr := r.db.DateOfLocal("event_time", p.TZOffsetMs, p.TZ)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS d, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND event_time >= ? AND event_time < ?
		GROUP BY d
		ORDER BY d`, dateExpr, filter),
		id, r.db.BindMillis(startMs), r.db.BindMillis(endMs))
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
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT d.virtual_key_id,
		       COALESCE(NULLIF(MAX(d.virtual_key_alias), ''), REPLACE(d.virtual_key_id, 'personal:', '')),
		       COALESCE(id.identity, ''),
		       COALESCE(SUM(d.total_tokens),0), COALESCE(SUM(d.request_count),0)
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
		GROUP BY d.virtual_key_id, id.identity
		ORDER BY SUM(d.total_tokens) DESC`, filter),
		startMsArg, endMsArg, id, startMsArg, endMsArg)
	if err != nil {
		return nil, fmt.Errorf("personal by-key total: %w", err)
	}
	defer rows.Close()

	var result []KeyTotal
	for rows.Next() {
		var kt KeyTotal
		if err := rows.Scan(&kt.VirtualKeyID, &kt.Alias, &kt.Identity, &kt.TotalTokens, &kt.RequestCount); err != nil {
			return nil, err
		}
		result = append(result, kt)
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
