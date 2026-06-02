package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// admin repository methods (cost-pricing Stage 3). They read/write the
// unpriced_models + pricing_snapshots tables and the DWD audit columns,
// all of which live in the SAME data DB as the usage facts (single SQLite
// file for Personal/Trial; the shared data Postgres for Production), so a
// plain query on r.db reaches them — no cross-service call.

// ListUnpricedModels — see Repository.ListUnpricedModels.
func (r *sqlRepo) ListUnpricedModels(ctx context.Context, status string) ([]UnpricedModel, error) {
	q := `SELECT model, provider, first_seen_at, last_seen_at, event_count, status, COALESCE(notes, '')
	      FROM unpriced_models`
	var args []interface{}
	if status != "" && status != "all" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	// event_count DESC puts the highest-impact missing prices on top
	// (matches the admin dashboard's default sort, design §7.3).
	q += ` ORDER BY event_count DESC, last_seen_at DESC`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list unpriced models: %w", err)
	}
	defer rows.Close()

	var out []UnpricedModel
	for rows.Next() {
		var m UnpricedModel
		if err := rows.Scan(&m.Model, &m.Provider, &m.FirstSeenAt, &m.LastSeenAt, &m.EventCount, &m.Status, &m.Notes); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateUnpricedModelStatus — see Repository.UpdateUnpricedModelStatus.
func (r *sqlRepo) UpdateUnpricedModelStatus(ctx context.Context, provider, model, status string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE unpriced_models SET status = ? WHERE provider = ? AND model = ?`,
		status, provider, model)
	if err != nil {
		return fmt.Errorf("update unpriced model status: %w", err)
	}
	// Distinguish "row didn't exist" from "updated" so the handler returns
	// 404 on a bad (provider, model) rather than a misleading 200.
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update unpriced model status (rows affected): %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetEventAudit — see Repository.GetEventAudit.
func (r *sqlRepo) GetEventAudit(ctx context.Context, eventID string) (*EventAudit, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT d.event_id,
		       COALESCE(d.model, ''),
		       COALESCE(d.provider_code, ''),
		       d.billable_amount,
		       COALESCE(d.currency, ''),
		       COALESCE(d.billing_period, ''),
		       COALESCE(d.region, ''),
		       COALESCE(d.endpoint_url, ''),
		       COALESCE(d.credential_id, ''),
		       COALESCE(d.pricing_snapshot_id, ''),
		       d.unit_prices_snapshot,
		       s.snapshot_id, s.litellm_sha256, s.history_sha256, s.overrides_sha256,
		       s.aikey_version, s.created_at, s.effective_from, s.effective_until
		FROM usage_fact_dwd AS d
		LEFT JOIN pricing_snapshots AS s ON s.snapshot_id = d.pricing_snapshot_id
		WHERE d.event_id = ?
		LIMIT 1`, eventID)

	var (
		a            EventAudit
		billable     sql.NullFloat64
		unitPrices   sql.NullString
		snapID       sql.NullString
		litellmSHA   sql.NullString
		historySHA   sql.NullString
		overridesSHA sql.NullString
		aikeyVer     sql.NullString
		snapCreated  sql.NullInt64
		snapFrom     sql.NullInt64
		snapUntil    sql.NullInt64
	)
	if err := row.Scan(
		&a.EventID, &a.Model, &a.ProviderCode,
		&billable, &a.Currency, &a.BillingPeriod, &a.Region, &a.EndpointURL,
		&a.CredentialID, &a.PricingSnapshotID, &unitPrices,
		&snapID, &litellmSHA, &historySHA, &overridesSHA,
		&aikeyVer, &snapCreated, &snapFrom, &snapUntil,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get event audit: %w", err)
	}

	if billable.Valid {
		a.BillableAmount = &billable.Float64
	}
	// unit_prices_snapshot is stored as a JSON string by the projector;
	// re-emit it as a nested object. Guard with json.Valid so a corrupt
	// value never breaks the whole response (leave nil instead).
	if unitPrices.Valid && unitPrices.String != "" && json.Valid([]byte(unitPrices.String)) {
		a.UnitPrices = json.RawMessage(unitPrices.String)
	}
	// Snapshot present only when the LEFT JOIN matched a snapshots row.
	if snapID.Valid {
		ps := &PricingSnapshot{
			SnapshotID:      snapID.String,
			LitellmSHA256:   litellmSHA.String,
			HistorySHA256:   historySHA.String,
			OverridesSHA256: overridesSHA.String,
			AikeyVersion:    aikeyVer.String,
			CreatedAt:       snapCreated.Int64,
			EffectiveFrom:   snapFrom.Int64,
		}
		if snapUntil.Valid {
			ps.EffectiveUntil = &snapUntil.Int64
		}
		a.Snapshot = ps
	}
	return &a, nil
}
