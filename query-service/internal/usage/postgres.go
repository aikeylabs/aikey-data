package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

type postgresRepo struct{ db *shared.DB }

func NewPostgresRepository(db *shared.DB) Repository {
	return &postgresRepo{db: db}
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

func (r *postgresRepo) PersonalTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error) {
	filter, id := personalFilter(p)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND usage_date BETWEEN ? AND ?
		GROUP BY usage_date
		ORDER BY usage_date`, r.db.DateString("usage_date"), filter),
		id, p.StartDate, p.EndDate)
	if err != nil {
		return nil, fmt.Errorf("personal timeline: %w", err)
	}
	defer rows.Close()
	return scanTimeline(rows)
}

// PersonalHourlyTimeline aggregates fact rows into 24 UTC hour buckets
// for p.StartDate. Dialect differences (Postgres EXTRACT vs SQLite
// strftime) are hidden behind shared.DB.HourBucket — see dbkit.go.
func (r *postgresRepo) PersonalHourlyTimeline(ctx context.Context, p QueryParams) ([]HourlyPoint, error) {
	filter, id := personalFilter(p)
	// Full-day window [startOfDay, startOfNextDay) in UTC. Using BETWEEN
	// on date alone (usage_date) would only give day-level totals; we
	// need event_time to keep intra-day resolution.
	dayStart := aikeytime.FromTime(p.StartDate.UTC().Truncate(24 * time.Hour))
	dayEnd := aikeytime.FromTime(p.StartDate.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour))

	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS hour, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND event_time >= ? AND event_time < ?
		GROUP BY hour
		ORDER BY hour`, r.db.HourBucket("event_time"), filter),
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

func (r *postgresRepo) PersonalByProtocolTimeline(ctx context.Context, p QueryParams) ([]ProtocolTimelinePoint, error) {
	filter, id := personalFilter(p)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s, COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND usage_date BETWEEN ? AND ?
		GROUP BY usage_date, COALESCE(provider_code, protocol_type)
		ORDER BY usage_date, COALESCE(provider_code, protocol_type)`, r.db.DateString("usage_date"), filter),
		id, p.StartDate, p.EndDate)
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

func (r *postgresRepo) PersonalByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error) {
	filter, id := personalFilter(p)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND usage_date BETWEEN ? AND ?
		GROUP BY COALESCE(provider_code, protocol_type)
		ORDER BY SUM(total_tokens) DESC`, filter),
		id, p.StartDate, p.EndDate)
	if err != nil {
		return nil, fmt.Errorf("personal by-protocol total: %w", err)
	}
	defer rows.Close()
	return scanProtocolTotal(rows)
}

func (r *postgresRepo) PersonalByKeyTotal(ctx context.Context, p QueryParams) ([]KeyTotal, error) {
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
	// DATE(event_time) worked while event_time was a TEXT ISO string, but
	// post v1.0.3-alpha the SQLite column is INTEGER millis. Calling
	// SQLite's DATE() on a bare integer returns NULL → the WHERE clause
	// matches zero rows → identity enrichment silently drops every row.
	// DateOf() encapsulates the right expression per dialect. See bugfix
	// 20260424 review finding #3.
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
		      AND %s BETWEEN ? AND ?
		    GROUP BY virtual_key_id
		) AS id ON id.virtual_key_id = d.virtual_key_id
		WHERE %s
		  AND d.usage_date BETWEEN ? AND ?
		GROUP BY d.virtual_key_id, id.identity
		ORDER BY SUM(d.total_tokens) DESC`, r.db.DateOf("event_time"), filter),
		p.StartDate, p.EndDate, id, p.StartDate, p.EndDate)
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

func (r *postgresRepo) MasterUserRanking(ctx context.Context, p QueryParams) ([]UserRanking, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT account_id, seat_id, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE org_id = ?
		  AND usage_date BETWEEN ? AND ?
		GROUP BY account_id, seat_id
		ORDER BY SUM(total_tokens) DESC
		LIMIT ?`,
		p.OrgID, p.StartDate, p.EndDate, p.Limit)
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

func (r *postgresRepo) MasterByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE org_id = ?
		  AND usage_date BETWEEN ? AND ?
		  AND billing_scope IN ('org_only','org_and_user')
		GROUP BY COALESCE(provider_code, protocol_type)
		ORDER BY SUM(total_tokens) DESC`,
		p.OrgID, p.StartDate, p.EndDate)
	if err != nil {
		return nil, fmt.Errorf("master by-protocol total: %w", err)
	}
	defer rows.Close()
	return scanProtocolTotal(rows)
}

func (r *postgresRepo) MasterTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE org_id = ?
		  AND usage_date BETWEEN ? AND ?
		  AND billing_scope IN ('org_only','org_and_user')
		GROUP BY usage_date
		ORDER BY usage_date`, r.db.DateString("usage_date")),
		p.OrgID, p.StartDate, p.EndDate)
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
