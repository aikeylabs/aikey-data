package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

type sqlODS struct{ db *shared.DB }

// NewSQLODSRepository creates an ODS repository backed by either
// PostgreSQL or SQLite — dialect differences are abstracted by
// shared.DB (see internal/shared/dbkit.go). Renamed from
// postgresODS/NewPostgresODSRepository 2026-04-24 because the file
// name implied PG-only but the implementation has always been
// dual-dialect via dbkit.
func NewSQLODSRepository(db *shared.DB) ODSRepository {
	return &sqlODS{db: db}
}

// ingest_received_at / collector_time are set explicitly from Go here
// (aikeytime.Now()) rather than relying on the SQL DEFAULT. Why: the
// v1.0.3-alpha SQLite migration's ADD COLUMN path does not preserve
// DEFAULT expressions, so an upgraded trial DB would leave these
// columns NULL if the write relied on the DEFAULT. Always binding from
// Go keeps upgraded and fresh installs behaviour-identical — the
// schema DEFAULT becomes redundant belt-and-braces. See bugfix
// 20260424 review finding #2.
const odsColumns = `event_id, request_id, trace_id, proxy_instance_id, device_id,
    schema_version, source_type, source_version, client_version,
    proxy_config_version, proxy_loaded_control_seq,
    event_time, occurred_at, started_at, finished_at,
    ingest_received_at, collector_time,
    org_id, account_id, seat_id, account_status_snapshot,
    virtual_key_id, virtual_key_revision, virtual_key_hash,
    binding_id, credential_id, credential_revision,
    real_key_hash, credential_fingerprint, provider_account_fingerprint,
    provider_id, provider_code, protocol_type, route_source, oauth_identity,
    model, request_count,
    input_tokens, output_tokens, cached_input_tokens, reasoning_tokens, total_tokens,
    billable_amount, currency,
    request_status, http_status_code, error_code, error_message, upstream_request_id,
    raw_usage_json, raw_headers_json, ext_json, raw_event_json`

const odsPlaceholders = `?,?,?,?,?,
    ?,?,?,?,
    ?,?,
    ?,?,?,?,
    ?,?,
    ?,?,?,?,
    ?,?,?,
    ?,?,?,
    ?,?,?,
    ?,?,?,?,?,
    ?,?,
    ?,?,?,?,?,
    ?,?,
    ?,?,?,?,?,
    ?,?,?,?`

func (r *sqlODS) InsertEvent(ctx context.Context, e *UsageEvent, rawJSON []byte) (bool, error) {
	insertODS := r.db.InsertOrIgnoreOn("usage_event_ods", odsColumns, odsPlaceholders, "org_id, event_id")
	res, err := r.db.ExecContext(ctx, insertODS,
		e.EventID, nullStr(e.RequestID), nullStr(e.TraceID), nullStr(e.ProxyInstanceID), nullStr(e.DeviceID),
		e.SchemaVersion, "local_proxy", nullStr(e.SourceVersion), nullStr(e.ClientVersion),
		nullStr(e.ProxyConfigVersion), e.ProxyLoadedControlSeq,
		r.db.BindMillis(e.EventTime), r.db.BindMillis(e.OccurredAt), r.db.BindMillisPtr(e.StartedAt), r.db.BindMillisPtr(e.FinishedAt),
		r.db.BindMillis(aikeytime.Now()), r.db.BindMillis(aikeytime.Now()),
		e.OrgID, nullStr(e.AccountID), nullStr(e.SeatID), nullStr(e.AccountStatusSnapshot),
		nullStr(e.VirtualKeyID), nullStr(e.VirtualKeyRevision), nullStr(e.VirtualKeyHash),
		nullStr(e.BindingID), nullStr(e.CredentialID), nullStr(e.CredentialRevision),
		nullStr(e.RealKeyHash), nullStr(e.CredentialFingerprint), nullStr(e.ProviderAccountFingerprint),
		nullStr(e.ProviderID), nullStr(e.ProviderCode), nullStr(e.ProtocolType), nullStr(e.RouteSource), nullStr(e.OAuthIdentity),
		nullStr(e.Model), e.RequestCount,
		e.InputTokens, e.OutputTokens, e.CachedInputTokens, e.ReasoningTokens, e.TotalTokens,
		nullStr(ptrStr(e.BillableAmount)), nullStr(e.Currency),
		e.RequestStatus, e.HTTPStatusCode, nullStr(e.ErrorCode), nullStr(e.ErrorMessage), nullStr(e.UpstreamRequestID),
		jsonbOrNull(e.RawUsageJSON), jsonbOrNull(e.RawHeadersJSON), jsonbOrNull(e.ExtJSON), rawJSON,
	)
	if err != nil {
		return false, fmt.Errorf("insert ods event %s: %w", e.EventID, err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, nil
	}
	// rowsAffected==0 from `INSERT OR IGNORE` / `ON CONFLICT DO NOTHING` is
	// historically misread as "duplicate". It also fires for *any* other
	// constraint violation that the IGNORE/DO NOTHING swallows (NOT NULL,
	// CHECK, FK, ...). Verify it's a genuine UNIQUE conflict before
	// reporting "duplicated"; otherwise surface a real error so the
	// caller can mark the event rejected with diagnostics rather than
	// returning HTTP 200 + duplicated:1 while data was silently dropped.
	// Discovered as F2 on 2026-04-27 during D-plan cross-version-jump
	// E2E: post-upgrade ODS NOT NULL on legacy_text columns swallowed
	// every fresh INSERT and reported them as duplicates. See
	// workflow/CI/bugfix/ for the regression record.
	var found int
	verr := r.db.QueryRowContext(ctx,
		"SELECT 1 FROM usage_event_ods WHERE org_id = ? AND event_id = ? LIMIT 1",
		e.OrgID, e.EventID,
	).Scan(&found)
	if verr == nil && found == 1 {
		return false, nil // genuine duplicate
	}
	if verr == sql.ErrNoRows {
		return false, fmt.Errorf("ods INSERT silently ignored (no UNIQUE conflict on org_id=%s event_id=%s) — likely NOT NULL/CHECK/FK violation. F2 root cause: NOT NULL on _legacy_text columns left over from v1.0.3 hook is hit when v1.0.4 collector binds only INTEGER columns. Run dbmigrate v1.0.4 hook upgrade to drop legacy NOT NULL", e.OrgID, e.EventID)
	}
	return false, fmt.Errorf("verify dedup for event_id=%s: %w", e.EventID, verr)
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullTime kept for backward compatibility with any callers that still bind
// *time.Time via sql.NullTime. New code goes through shared.DB.BindMillis.
// Unused placeholder import below ensures aikeytime is wired in.
var _ = aikeytime.Millis(0)

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func jsonbOrNull(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
