package projector

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// --- ODSReader ---

type postgresODSReader struct{ db *shared.DB }

func NewPostgresODSReader(db *shared.DB) ODSReader { return &postgresODSReader{db: db} }

// fetchPendingSQL is built dynamically via nowExpr for dialect portability.
const fetchPendingTpl = `
SELECT ods_id, event_id, event_time, occurred_at,
       org_id, account_id, seat_id, account_status_snapshot,
       virtual_key_id, virtual_key_revision, virtual_key_hash,
       binding_id, credential_id, credential_revision,
       real_key_hash, credential_fingerprint, provider_account_fingerprint,
       provider_id, provider_code, protocol_type, route_source,
       model, request_count,
       input_tokens, output_tokens, cached_input_tokens, reasoning_tokens, total_tokens,
       billable_amount, currency,
       request_status, http_status_code, upstream_request_id,
       dwd_retry_count
FROM usage_event_ods
WHERE (dwd_status = 'pending')
   OR (dwd_status = 'retry' AND dwd_next_retry_at <= %s)
ORDER BY ods_id
LIMIT ?
`

func (r *postgresODSReader) FetchPending(ctx context.Context, limit int) ([]ODSRecord, error) {
	query := fmt.Sprintf(fetchPendingTpl, r.db.Now())
	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch pending ods: %w", err)
	}
	defer rows.Close()

	var records []ODSRecord
	for rows.Next() {
		var rec ODSRecord
		if err := rows.Scan(
			&rec.OdsID, &rec.EventID, &rec.EventTime, &rec.OccurredAt,
			&rec.OrgID, &rec.AccountID, &rec.SeatID, &rec.AccountStatusSnapshot,
			&rec.VirtualKeyID, &rec.VirtualKeyRevision, &rec.VirtualKeyHash,
			&rec.BindingID, &rec.CredentialID, &rec.CredentialRevision,
			&rec.RealKeyHash, &rec.CredentialFingerprint, &rec.ProviderAccountFingerprint,
			&rec.ProviderID, &rec.ProviderCode, &rec.ProtocolType, &rec.RouteSource,
			&rec.Model, &rec.RequestCount,
			&rec.InputTokens, &rec.OutputTokens, &rec.CachedInputTokens, &rec.ReasoningTokens, &rec.TotalTokens,
			&rec.BillableAmount, &rec.Currency,
			&rec.RequestStatus, &rec.HTTPStatusCode, &rec.UpstreamRequestID,
			&rec.DwdRetryCount,
		); err != nil {
			return nil, fmt.Errorf("scan ods row: %w", err)
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (r *postgresODSReader) MarkProjected(ctx context.Context, odsID int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE usage_event_ods SET dwd_status = 'projected' WHERE ods_id = ?`, odsID)
	return err
}

func (r *postgresODSReader) MarkRetry(ctx context.Context, odsID int64, retryCount int, errCode, errMsg string) error {
	// Compute retry time in Go to avoid PG-specific interval arithmetic.
	nextRetryAt := time.Now().Add(retryDelay(retryCount))
	_, err := r.db.ExecContext(ctx,
		`UPDATE usage_event_ods
		 SET dwd_status = 'retry',
		     dwd_retry_count = ?,
		     dwd_next_retry_at = ?,
		     dwd_last_error_code = ?,
		     dwd_last_error_msg = ?
		 WHERE ods_id = ?`,
		retryCount, nextRetryAt, errCode, errMsg, odsID)
	return err
}

func (r *postgresODSReader) MarkDeadLetter(ctx context.Context, odsID int64, errCode, errMsg string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE usage_event_ods
		 SET dwd_status = 'dead_letter',
		     dwd_last_error_code = ?,
		     dwd_last_error_msg = ?
		 WHERE ods_id = ?`,
		odsID, errCode, errMsg)
	return err
}

// retryDelay returns the delay before the next retry.
// 1-3: 1min, 4-10: 10min, 11+: 1hr
func retryDelay(retryCount int) time.Duration {
	switch {
	case retryCount <= 3:
		return 1 * time.Minute
	case retryCount <= 10:
		return 10 * time.Minute
	default:
		return 1 * time.Hour
	}
}

// --- ControlEventReader ---

type postgresControlEventReader struct{ db *shared.DB }

func NewPostgresControlEventReader(db *shared.DB) ControlEventReader {
	return &postgresControlEventReader{db: db}
}

const findControlEventSQL = `
SELECT event_id, org_id, account_id, change_type, entity_type,
       seat_id, virtual_key_id, virtual_key_revision,
       binding_id, credential_id, credential_revision,
       revision, provider_id, effective_from, effective_to,
       after_snapshot_json
FROM managed_key_control_events
WHERE virtual_key_id = ?
  AND effective_from <= ?
  AND (effective_to IS NULL OR effective_to > ?)
ORDER BY effective_from DESC
LIMIT 1
`

func (r *postgresControlEventReader) FindByVirtualKeyAtTime(ctx context.Context, virtualKeyID string, eventTime interface{}) (*ControlEvent, error) {
	row := r.db.QueryRowContext(ctx, findControlEventSQL, virtualKeyID, eventTime, eventTime)
	var ce ControlEvent
	err := row.Scan(
		&ce.EventID, &ce.OrgID, &ce.AccountID, &ce.ChangeType, &ce.EntityType,
		&ce.SeatID, &ce.VirtualKeyID, &ce.VirtualKeyRevision,
		&ce.BindingID, &ce.CredentialID, &ce.CredentialRevision,
		&ce.Revision, &ce.ProviderID, &ce.EffectiveFrom, &ce.EffectiveTo,
		&ce.AfterSnapshotJSON,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find control event for vk=%s: %w", virtualKeyID, err)
	}
	return &ce, nil
}

// --- DWDWriter ---

type postgresDWDWriter struct{ db *shared.DB }

func NewPostgresDWDWriter(db *shared.DB) DWDWriter { return &postgresDWDWriter{db: db} }

const dwdColumns = `event_id, ods_id, occurred_at, event_time, usage_date,
    org_id, account_id, seat_id,
    virtual_key_id, virtual_key_revision, virtual_key_alias, virtual_key_hash,
    binding_id, binding_alias,
    credential_id, credential_revision, real_key_hash,
    credential_fingerprint, provider_account_fingerprint,
    provider_id, provider_code, provider_display_name, protocol_type,
    route_source, model,
    request_count, input_tokens, output_tokens, cached_input_tokens,
    reasoning_tokens, total_tokens, billable_amount, currency,
    request_status, http_status_code, upstream_request_id,
    completion_source, quality_status, validation_code, validation_message,
    anomaly_type, anomaly_reason, billing_scope, user_usage_scope,
    control_event_id, control_event_revision, projector_version`

const dwdPlaceholders = `?,?,?,?,?,
    ?,?,?,
    ?,?,?,?,
    ?,?,
    ?,?,?,
    ?,?,
    ?,?,?,?,
    ?,?,
    ?,?,?,?,
    ?,?,?,?,
    ?,?,?,
    ?,?,?,?,
    ?,?,?,?,
    ?,?,?`

func (w *postgresDWDWriter) Insert(ctx context.Context, f *DWDFact) (bool, error) {
	insertDWDSQL := w.db.InsertOrIgnoreOn("usage_fact_dwd", dwdColumns, dwdPlaceholders, "org_id, event_id")
	res, err := w.db.ExecContext(ctx, insertDWDSQL,
		// Why: SQLite stores time.Time as its default String() format (e.g. "2026-04-08 08:00:00 +0800 CST")
		// which breaks date range queries. Format UsageDate as ISO date for cross-dialect compatibility.
		f.EventID, f.OdsID, f.OccurredAt, f.EventTime, f.UsageDate.Format("2006-01-02"),
		f.OrgID, f.AccountID, f.SeatID,
		f.VirtualKeyID, f.VirtualKeyRevision, f.VirtualKeyAlias, f.VirtualKeyHash,
		f.BindingID, f.BindingAlias,
		f.CredentialID, f.CredentialRevision, f.RealKeyHash,
		f.CredentialFingerprint, f.ProviderAccountFingerprint,
		f.ProviderID, f.ProviderCode, f.ProviderDisplayName, f.ProtocolType,
		f.RouteSource, f.Model,
		f.RequestCount, f.InputTokens, f.OutputTokens, f.CachedInputTokens,
		f.ReasoningTokens, f.TotalTokens, f.BillableAmount, f.Currency,
		f.RequestStatus, f.HTTPStatusCode, f.UpstreamRequestID,
		f.CompletionSource, string(f.QualityStatus), f.ValidationCode, f.ValidationMessage,
		string(f.AnomalyType), f.AnomalyReason, string(f.BillingScope), string(f.UserUsageScope),
		f.ControlEventID, f.ControlEventRevision, f.ProjectorVersion,
	)
	if err != nil {
		return false, fmt.Errorf("insert dwd fact %s: %w", f.EventID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// --- CheckpointStore ---

type postgresCheckpointStore struct{ db *shared.DB }

func NewPostgresCheckpointStore(db *shared.DB) CheckpointStore {
	return &postgresCheckpointStore{db: db}
}

func (s *postgresCheckpointStore) GetLastScannedOdsID(ctx context.Context, taskName string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT last_scanned_ods_id FROM usage_dwd_projector_tasks WHERE task_name = ?`,
		taskName).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (s *postgresCheckpointStore) UpdateCheckpoint(ctx context.Context, taskName string, lastOdsID int64) error {
	nowExpr := s.db.Now()
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE usage_dwd_projector_tasks
		 SET last_scanned_ods_id = ?, last_scanned_at = %s, last_success_at = %s, updated_at = %s
		 WHERE task_name = ?`, nowExpr, nowExpr, nowExpr),
		lastOdsID, taskName)
	return err
}
