package usage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

type postgresRepo struct{ db *shared.DB }

func NewPostgresRepository(db *shared.DB) Repository {
	return &postgresRepo{db: db}
}

// personalFilter returns the WHERE clause and parameter for personal queries.
// If SeatID is set, filter by seat_id; otherwise fall back to account_id.
// For personal/BYOK keys that bypass control-plane verification, we include
// all billing_scope values (not just user_usage_scope='normal') so personal
// usage data is always visible.
func personalFilter(p QueryParams) (clause string, id string) {
	if p.SeatID != "" {
		return "seat_id = ?", p.SeatID
	}
	return "account_id = ?", p.AccountID
}

// --- Personal page ---

func (r *postgresRepo) PersonalTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error) {
	filter, id := personalFilter(p)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT usage_date::text, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND usage_date BETWEEN ? AND ?
		GROUP BY usage_date
		ORDER BY usage_date`, filter),
		id, p.StartDate, p.EndDate)
	if err != nil {
		return nil, fmt.Errorf("personal timeline: %w", err)
	}
	defer rows.Close()
	return scanTimeline(rows)
}

func (r *postgresRepo) PersonalByProtocolTimeline(ctx context.Context, p QueryParams) ([]ProtocolTimelinePoint, error) {
	filter, id := personalFilter(p)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT usage_date::text, COALESCE(provider_code, protocol_type), COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND usage_date BETWEEN ? AND ?
		GROUP BY usage_date, COALESCE(provider_code, protocol_type)
		ORDER BY usage_date, COALESCE(provider_code, protocol_type)`, filter),
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
	filter, id := personalFilter(p)
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT virtual_key_id,
		       COALESCE(NULLIF(MAX(virtual_key_alias), ''), REGEXP_REPLACE(virtual_key_id, '^personal:', '')),
		       COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE %s
		  AND usage_date BETWEEN ? AND ?
		GROUP BY virtual_key_id
		ORDER BY SUM(total_tokens) DESC`, filter),
		id, p.StartDate, p.EndDate)
	if err != nil {
		return nil, fmt.Errorf("personal by-key total: %w", err)
	}
	defer rows.Close()

	var result []KeyTotal
	for rows.Next() {
		var kt KeyTotal
		if err := rows.Scan(&kt.VirtualKeyID, &kt.Alias, &kt.TotalTokens, &kt.RequestCount); err != nil {
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
	rows, err := r.db.QueryContext(ctx, `
		SELECT usage_date::text, COALESCE(SUM(total_tokens),0), COALESCE(SUM(request_count),0)
		FROM usage_fact_dwd
		WHERE org_id = ?
		  AND usage_date BETWEEN ? AND ?
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
