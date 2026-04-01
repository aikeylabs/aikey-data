package usage

import (
	"context"
	"database/sql"
	"fmt"
)

type postgresRepo struct{ db *sql.DB }

func NewPostgresRepository(db *sql.DB) Repository {
	return &postgresRepo{db: db}
}

// --- Personal page ---

func (r *postgresRepo) PersonalTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT usage_date::text, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE seat_id = $1
		  AND usage_date BETWEEN $2 AND $3
		  AND user_usage_scope = 'normal'
		GROUP BY usage_date
		ORDER BY usage_date`,
		p.SeatID, p.StartDate, p.EndDate)
	if err != nil {
		return nil, fmt.Errorf("personal timeline: %w", err)
	}
	defer rows.Close()
	return scanTimeline(rows)
}

func (r *postgresRepo) PersonalByProtocolTimeline(ctx context.Context, p QueryParams) ([]ProtocolTimelinePoint, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT usage_date::text, protocol_type, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE seat_id = $1
		  AND usage_date BETWEEN $2 AND $3
		  AND user_usage_scope = 'normal'
		GROUP BY usage_date, protocol_type
		ORDER BY usage_date, protocol_type`,
		p.SeatID, p.StartDate, p.EndDate)
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
	rows, err := r.db.QueryContext(ctx, `
		SELECT protocol_type, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE seat_id = $1
		  AND usage_date BETWEEN $2 AND $3
		  AND user_usage_scope = 'normal'
		GROUP BY protocol_type
		ORDER BY SUM(total_tokens) DESC`,
		p.SeatID, p.StartDate, p.EndDate)
	if err != nil {
		return nil, fmt.Errorf("personal by-protocol total: %w", err)
	}
	defer rows.Close()
	return scanProtocolTotal(rows)
}

// --- Master page ---

func (r *postgresRepo) MasterUserRanking(ctx context.Context, p QueryParams) ([]UserRanking, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT account_id, seat_id, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE org_id = $1
		  AND usage_date BETWEEN $2 AND $3
		  AND user_usage_scope = 'normal'
		GROUP BY account_id, seat_id
		ORDER BY SUM(total_tokens) DESC
		LIMIT $4`,
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
		SELECT protocol_type, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE org_id = $1
		  AND usage_date BETWEEN $2 AND $3
		  AND billing_scope IN ('org_only','org_and_user')
		GROUP BY protocol_type
		ORDER BY SUM(total_tokens) DESC`,
		p.OrgID, p.StartDate, p.EndDate)
	if err != nil {
		return nil, fmt.Errorf("master by-protocol total: %w", err)
	}
	defer rows.Close()
	return scanProtocolTotal(rows)
}

func (r *postgresRepo) MasterTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT usage_date::text, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE org_id = $1
		  AND usage_date BETWEEN $2 AND $3
		  AND billing_scope IN ('org_only','org_and_user')
		GROUP BY usage_date
		ORDER BY usage_date`,
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
