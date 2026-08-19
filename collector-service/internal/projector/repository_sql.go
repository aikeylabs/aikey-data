package projector

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

// --- ODSReader ---

// sqlODSReader executes through ex — the autocommit pool by default, or a
// batch transaction when the worker runs the P0-4 single-tx scan path. db is
// kept for dialect helpers (pure string/bind builders).
type sqlODSReader struct {
	db *shared.DB
	ex shared.Execer
}

func NewSQLODSReader(db *shared.DB) ODSReader { return &sqlODSReader{db: db, ex: db} }

// fetchPendingSQL is built dynamically via nowExpr for dialect portability.
//
// Why no canary filter: canary events flow through the projector as a liveness
// signal so that diagnostics can observe end-to-end traversal (the proxy's
// canary probe and /internal/canary-check both rely on canary ODS reaching
// the 'projected' dwd_status). The projector worker short-circuits canary
// events to MarkProjected without writing to usage_fact_dwd, so they never
// pollute business stats. See worker.go:projectOne.
const fetchPendingTpl = `
SELECT ods_id, event_id, request_id, event_time, occurred_at,
       org_id, account_id, seat_id, account_status_snapshot,
       virtual_key_id, virtual_key_revision, virtual_key_hash, virtual_key_alias,
       binding_id, credential_id, credential_revision,
       real_key_hash, credential_fingerprint, provider_account_fingerprint,
       provider_id, provider_code, protocol_type, route_source,
       model, request_count,
       input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens, reasoning_tokens, total_tokens,
       billable_amount, currency,
       request_status, http_status_code, upstream_request_id,
       dwd_retry_count,
       app_slug,
       session_id,
       region, endpoint_url,
       oauth_identity,
       content_hash, source_id, source_seq,
       fallback_reason, fallback_attempt,
       %s AS request_path
FROM usage_event_ods
WHERE ((dwd_status = 'pending')
   OR (dwd_status = 'retry' AND dwd_next_retry_at <= %s))
ORDER BY ods_id
LIMIT ?
`

func (r *sqlODSReader) FetchPending(ctx context.Context, limit int) ([]ODSRecord, error) {
	// dwd_next_retry_at is an int64-millis column on SQLite (v1.0.3-alpha)
	// and TIMESTAMPTZ on Postgres. Compare against NowMillis() which
	// produces the right per-dialect expression. Using plain Now() here
	// would emit datetime('now') on SQLite, and a TEXT-to-INTEGER
	// lexicographic compare would match every retry row on every scan
	// (hot loop). See bugfix 20260424 review finding #1.
	// request_path lives inside raw_event_json (additive wire field, no ODS
	// column — see 20260715-非生成流量不进用量审计与统计.md). Extracted
	// SQL-side so we don't ship the whole raw blob per row.
	query := fmt.Sprintf(fetchPendingTpl, r.db.JSONText("raw_event_json", "request_path"), r.db.NowMillis())
	rows, err := r.ex.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch pending ods: %w", err)
	}
	defer rows.Close()

	var records []ODSRecord
	for rows.Next() {
		var rec ODSRecord
		if err := rows.Scan(
			&rec.OdsID, &rec.EventID, &rec.RequestID, &rec.EventTime, &rec.OccurredAt,
			&rec.OrgID, &rec.AccountID, &rec.SeatID, &rec.AccountStatusSnapshot,
			&rec.VirtualKeyID, &rec.VirtualKeyRevision, &rec.VirtualKeyHash, &rec.VirtualKeyAlias,
			&rec.BindingID, &rec.CredentialID, &rec.CredentialRevision,
			&rec.RealKeyHash, &rec.CredentialFingerprint, &rec.ProviderAccountFingerprint,
			&rec.ProviderID, &rec.ProviderCode, &rec.ProtocolType, &rec.RouteSource,
			&rec.Model, &rec.RequestCount,
			&rec.InputTokens, &rec.OutputTokens, &rec.CachedInputTokens, &rec.CacheCreationInputTokens, &rec.ReasoningTokens, &rec.TotalTokens,
			&rec.BillableAmount, &rec.Currency,
			&rec.RequestStatus, &rec.HTTPStatusCode, &rec.UpstreamRequestID,
			&rec.DwdRetryCount,
			&rec.AppSlug,
			&rec.SessionID,
			&rec.Region, &rec.EndpointURL,
			&rec.OAuthIdentity,
			&rec.ContentHash, &rec.SourceID, &rec.SourceSeq,
			&rec.FallbackReason, &rec.FallbackAttempt,
			&rec.RequestPath,
		); err != nil {
			return nil, fmt.Errorf("scan ods row: %w", err)
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (r *sqlODSReader) MarkProjected(ctx context.Context, odsID int64, eventTime aikeytime.Millis) error {
	// event_time in the WHERE prunes to the one partition on PostgreSQL (ODS is
	// partitioned by event_time, v1.0.1-alpha.4) — without it this per-event
	// UPDATE would scan every monthly partition. Harmless extra filter on SQLite.
	_, err := r.ex.ExecContext(ctx,
		`UPDATE usage_event_ods SET dwd_status = 'projected' WHERE ods_id = ? AND event_time = ?`,
		odsID, r.db.BindMillis(eventTime))
	return err
}

func (r *sqlODSReader) MarkRetry(ctx context.Context, odsID int64, eventTime aikeytime.Millis, retryCount int, errCode, errMsg string) error {
	// Compute retry time in Go to avoid PG-specific interval arithmetic.
	// Wrap as aikeytime.Millis so the dialect-aware bind helper emits the
	// right driver type (int64 for SQLite INTEGER, time.Time for PG TIMESTAMPTZ).
	nextRetryAt := aikeytime.FromTime(time.Now().Add(retryDelay(retryCount)))
	// event_time prunes to one partition on PG (see MarkProjected).
	_, err := r.ex.ExecContext(ctx,
		`UPDATE usage_event_ods
		 SET dwd_status = 'retry',
		     dwd_retry_count = ?,
		     dwd_next_retry_at = ?,
		     dwd_last_error_code = ?,
		     dwd_last_error_msg = ?
		 WHERE ods_id = ? AND event_time = ?`,
		retryCount, r.db.BindMillis(nextRetryAt), errCode, errMsg, odsID, r.db.BindMillis(eventTime))
	return err
}

func (r *sqlODSReader) MarkDeadLetter(ctx context.Context, odsID int64, eventTime aikeytime.Millis, errCode, errMsg string) error {
	// Bind order matches the three `?` placeholders by POSITION (SQLite/PG
	// shared dialect): code → msg → ods_id. The original 4/8 shared.DB
	// refactor (commit 39a8e526) translated PG named placeholders ($1=odsID,
	// $2=errCode, $3=errMsg) into `?` but kept the old PG-order argument
	// list (odsID, errCode, errMsg). Result: `WHERE ods_id = '<errMsg>'`
	// matched zero rows, so retried events never transitioned to
	// 'dead_letter' and `FetchPending` re-fetched them every scan → the
	// projector worker stalled at the first permanent-error event and
	// never advanced `last_scanned_ods_id`. See bugfix 20260522.
	// event_time prunes to one partition on PG (see MarkProjected). Bind order
	// follows the `?` positions: code → msg → ods_id → event_time.
	_, err := r.ex.ExecContext(ctx,
		`UPDATE usage_event_ods
		 SET dwd_status = 'dead_letter',
		     dwd_last_error_code = ?,
		     dwd_last_error_msg = ?
		 WHERE ods_id = ? AND event_time = ?`,
		errCode, errMsg, odsID, r.db.BindMillis(eventTime))
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

type sqlControlEventReader struct{ db *shared.DB }

func NewSQLControlEventReader(db *shared.DB) ControlEventReader {
	return &sqlControlEventReader{db: db}
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

func (r *sqlControlEventReader) FindByVirtualKeyAtTime(ctx context.Context, virtualKeyID string, eventTime aikeytime.Millis) (*ControlEvent, error) {
	bound := r.db.BindMillis(eventTime)
	row := r.db.QueryRowContext(ctx, findControlEventSQL, virtualKeyID, bound, bound)
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

type sqlDWDWriter struct {
	db *shared.DB
	ex shared.Execer
}

func NewSQLDWDWriter(db *shared.DB) DWDWriter { return &sqlDWDWriter{db: db, ex: db} }

// projected_at is set explicitly from Go (aikeytime.Now()) rather than
// relying on the SQL DEFAULT. Why: the v1.0.3-alpha migration's
// ADD COLUMN path drops the DEFAULT expression, so an upgraded trial
// DB would leave projected_at NULL on new inserts. Always binding from
// Go makes upgraded and fresh installs behaviour-identical. See
// bugfix 20260424 review finding #2.
// Column order: keep new columns at the tail (after app_slug) so
// historic positions stay fixed. v1.0.0-rc.5 added app_slug for Phase
// 4 Connected Apps; v1.0.0-rc.6 adds session_id for the Performance
// Top N sessions chart.
const dwdColumns = `event_id, request_id, ods_id, occurred_at, event_time, usage_date,
    org_id, account_id, seat_id,
    virtual_key_id, virtual_key_revision, virtual_key_alias, virtual_key_hash,
    binding_id, binding_alias,
    credential_id, credential_revision, real_key_hash,
    credential_fingerprint, provider_account_fingerprint,
    provider_id, provider_code, provider_display_name, protocol_type,
    route_source, model,
    request_count, input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens,
    reasoning_tokens, total_tokens, billable_amount, currency,
    request_status, http_status_code, upstream_request_id,
    completion_source, quality_status, validation_code, validation_message,
    anomaly_type, anomaly_reason, billing_scope, user_usage_scope,
    control_event_id, control_event_revision, projector_version, projected_at,
    app_slug, session_id,
    region, endpoint_url, billing_period, unit_prices_snapshot, pricing_snapshot_id,
    oauth_identity,
    content_hash, source_id, source_seq,
    fallback_reason, fallback_attempt`

const dwdPlaceholders = `?,?,?,?,?,?,
    ?,?,?,
    ?,?,?,?,
    ?,?,
    ?,?,?,
    ?,?,
    ?,?,?,?,
    ?,?,
    ?,?,?,?,?,
    ?,?,?,?,
    ?,?,?,
    ?,?,?,?,
    ?,?,?,?,
    ?,?,?,?,
    ?,?,
    ?,?,?,?,?,
    ?,
    ?,?,?,
    ?,?`

func (w *sqlDWDWriter) Insert(ctx context.Context, f *DWDFact) (bool, error) {
	// Dialect-aware conflict target: PostgreSQL's usage_fact_dwd is partitioned
	// by usage_date (v1.0.1-alpha.4), so its UNIQUE constraint is
	// (org_id, event_id, usage_date) — the partition key must be in the
	// constraint, hence in the ON CONFLICT inference. SQLite is not partitioned
	// and uses INSERT OR IGNORE (conflictTarget ignored), so the plain
	// (org_id, event_id) UNIQUE still applies. Both are dedup-equivalent because
	// usage_date is deterministic from event_time (stamped once, never changes).
	conflictTarget := "org_id, event_id"
	if w.db.Dialect == shared.DialectPostgres {
		conflictTarget = "org_id, event_id, usage_date"
	}
	insertDWDSQL := w.db.InsertOrIgnoreOn("usage_fact_dwd", dwdColumns, dwdPlaceholders, conflictTarget)
	res, err := w.ex.ExecContext(ctx, insertDWDSQL,
		// Why int64 millis (via BindMillis) instead of time.Time: Go's default
		// time.Time String() format contains a local tz suffix (e.g. "+0800
		// CST") that SQLite's date functions cannot parse, which broke
		// strftime-based hour bucketing (see bugfix 20260424). β-hybrid on
		// Postgres: BindMillis returns time.Time so TIMESTAMPTZ still works.
		// UsageDate stays as an ISO "YYYY-MM-DD" string for BETWEEN queries.
		f.EventID, f.RequestID, f.OdsID, w.db.BindMillis(f.OccurredAt), w.db.BindMillis(f.EventTime), f.UsageDate,
		f.OrgID, f.AccountID, f.SeatID,
		f.VirtualKeyID, f.VirtualKeyRevision, f.VirtualKeyAlias, f.VirtualKeyHash,
		f.BindingID, f.BindingAlias,
		f.CredentialID, f.CredentialRevision, f.RealKeyHash,
		f.CredentialFingerprint, f.ProviderAccountFingerprint,
		f.ProviderID, f.ProviderCode, f.ProviderDisplayName, f.ProtocolType,
		f.RouteSource, f.Model,
		f.RequestCount, f.InputTokens, f.OutputTokens, f.CachedInputTokens, f.CacheCreationInputTokens,
		f.ReasoningTokens, f.TotalTokens, f.BillableAmount, f.Currency,
		f.RequestStatus, f.HTTPStatusCode, f.UpstreamRequestID,
		f.CompletionSource, string(f.QualityStatus), f.ValidationCode, f.ValidationMessage,
		string(f.AnomalyType), f.AnomalyReason, string(f.BillingScope), string(f.UserUsageScope),
		f.ControlEventID, f.ControlEventRevision, f.ProjectorVersion, w.db.BindMillis(aikeytime.Now()),
		// AppSlug stays as plain string — the partial index treats
		// `''` and NULL identically, and other "may be empty" fields
		// in this insert (BindingID, AccountID, etc.) follow the same
		// pattern. Avoids needing a dialect-aware null-or-empty helper.
		f.AppSlug,
		// SessionID (v1.0.0-rc.6): same empty-vs-NULL ambivalence as
		// AppSlug — by-session SQL coalesces NULL to '' so both
		// surface identically downstream. Empty string is the common
		// case (most requests carry no session marker).
		f.SessionID,
		// Cost-pricing audit (v1.0.0-rc.8): region/endpoint passthrough +
		// projector-computed cost trail (billable_amount/currency bound above
		// are now enricher-computed, not passthrough).
		f.Region, f.EndpointURL, f.BillingPeriod, f.UnitPricesSnapshot, f.PricingSnapshotID,
		// oauth_identity (v1.0.1-alpha.1): carried ODS→DWD so the read model can filter
		// by OAuth email directly (was dropped during projection — see the
		// PersonalByKeyTotal ODS-join hack this removes the need for).
		f.OAuthIdentity,
		// Delivery-integrity passthrough (v1.0.1-alpha.3): content_hash/source_id/
		// source_seq carried ODS→DWD for the usage-audit export's tamper/gap
		// evidence. SourceSeq is *int64 → binds SQL NULL for old-proxy events.
		f.ContentHash, f.SourceID, f.SourceSeq,
		// Upstream fallback attribution: which hop served this row, and what sent
		// us to it. Carried verbatim ODS→DWD so the console can aggregate over the
		// read model — the LIVE cooldown table never leaves the developer's
		// machine (I23), so a count of past switches is the only thing the console
		// may be shown. FallbackAttempt is *int64 → binds SQL NULL for rows
		// written before the field existed.
		f.FallbackReason, f.FallbackAttempt,
	)
	if err != nil {
		return false, fmt.Errorf("insert dwd fact %s: %w", f.EventID, err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, nil
	}
	// Same defense-in-depth as ods InsertEvent (see F2 fix in
	// internal/ingest/repository_sql.go). rowsAffected==0 from
	// `INSERT OR IGNORE` / `ON CONFLICT DO NOTHING` is ambiguous —
	// verify it's a real duplicate via SELECT before silently dropping
	// the projection.
	var found int
	// usage_date in the WHERE prunes to the one partition on PostgreSQL (and is
	// a harmless extra filter on SQLite) — without it this dedup-verify would
	// scan every monthly partition. usage_date is deterministic per event so it
	// cannot exclude a genuine duplicate.
	verr := w.ex.QueryRowContext(ctx,
		"SELECT 1 FROM usage_fact_dwd WHERE org_id = ? AND event_id = ? AND usage_date = ? LIMIT 1",
		f.OrgID, f.EventID, f.UsageDate,
	).Scan(&found)
	if verr == nil && found == 1 {
		return false, nil // genuine duplicate
	}
	if verr == sql.ErrNoRows {
		return false, fmt.Errorf("dwd INSERT silently ignored (no UNIQUE conflict on org_id=%s event_id=%s) — likely NOT NULL/CHECK/FK violation; check schema vs DWDFact column list", f.OrgID, f.EventID)
	}
	return false, fmt.Errorf("verify dedup for dwd event_id=%s: %w", f.EventID, verr)
}

// --- CheckpointStore ---

type sqlCheckpointStore struct {
	db *shared.DB
	ex shared.Execer
}

func NewSQLCheckpointStore(db *shared.DB) CheckpointStore {
	return &sqlCheckpointStore{db: db, ex: db}
}

func (s *sqlCheckpointStore) GetLastScannedOdsID(ctx context.Context, taskName string) (int64, error) {
	var id int64
	err := s.ex.QueryRowContext(ctx,
		`SELECT last_scanned_ods_id FROM usage_dwd_projector_tasks WHERE task_name = ?`,
		taskName).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (s *sqlCheckpointStore) UpdateCheckpoint(ctx context.Context, taskName string, lastOdsID int64) error {
	nowExpr := s.db.Now()
	_, err := s.ex.ExecContext(ctx,
		fmt.Sprintf(`UPDATE usage_dwd_projector_tasks
		 SET last_scanned_ods_id = ?, last_scanned_at = %s, last_success_at = %s, updated_at = %s
		 WHERE task_name = ?`, nowExpr, nowExpr, nowExpr),
		lastOdsID, taskName)
	return err
}
