package projector

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

// TestFetchPending_SQLiteRetryScheduling is the regression guard for
// bugfix 20260424 review finding #1. Before the fix, the retry
// WHERE-clause compared an INTEGER millis column against SQLite's
// datetime('now') TEXT output:
//
//	dwd_next_retry_at <= datetime('now')
//	-- int64 on LHS, text on RHS → lexicographic compare
//	-- "1777041000000" < "2026-04-24 05:13:34" → TRUE for every row
//
// …so every retry row would be re-fetched on every scan, producing a
// hot loop instead of honoring the exponential-backoff window.
//
// After the fix, FetchPending uses NowMillis() which evaluates to the
// current time in the same representation as the column (INTEGER
// millis on SQLite) — comparison is numeric, future retry times are
// correctly excluded.
func TestFetchPending_SQLiteRetryScheduling(t *testing.T) {
	db := newSQLiteODSTestDB(t)

	now := aikeytime.Now()
	pastRetry := aikeytime.FromTime(now.Time().Add(-5 * time.Minute))
	futureRetry := aikeytime.FromTime(now.Time().Add(10 * time.Minute))

	// Three rows:
	//   1: pending — always due.
	//   2: retry with next_retry_at in the past — due.
	//   3: retry with next_retry_at in the future — NOT due; this is the
	//      row that the bug would have wrongly surfaced.
	insertODSTestRow(t, db, 1, "pending", aikeytime.Millis(0))
	insertODSTestRow(t, db, 2, "retry", pastRetry)
	insertODSTestRow(t, db, 3, "retry", futureRetry)

	reader := NewSQLODSReader(db)
	pending, err := reader.FetchPending(context.Background(), 100)
	if err != nil {
		t.Fatalf("FetchPending: %v", err)
	}

	var fetchedIDs []int64
	for _, p := range pending {
		fetchedIDs = append(fetchedIDs, p.OdsID)
	}

	// Must contain 1 and 2, must NOT contain 3.
	gotFuture := false
	gotPast := false
	gotPending := false
	for _, id := range fetchedIDs {
		switch id {
		case 1:
			gotPending = true
		case 2:
			gotPast = true
		case 3:
			gotFuture = true
		}
	}
	if !gotPending {
		t.Errorf("pending row (ods_id=1) missing from FetchPending result: %v", fetchedIDs)
	}
	if !gotPast {
		t.Errorf("past-due retry row (ods_id=2) missing from FetchPending result: %v", fetchedIDs)
	}
	if gotFuture {
		t.Errorf("future retry row (ods_id=3) was wrongly returned by FetchPending — retry hot-loop regression: %v", fetchedIDs)
	}
}

// newSQLiteODSTestDB spins up an in-memory SQLite DB with the minimal
// ODS schema needed by FetchPending. Mirrors the real schema's
// INTEGER timestamp affinity post v1.0.3-alpha.
func newSQLiteODSTestDB(t *testing.T) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	// Column set mirrors the SELECT in fetchPendingTpl so scans line up.
	_, err = raw.Exec(`
		CREATE TABLE usage_event_ods (
			ods_id INTEGER PRIMARY KEY,
			event_id TEXT,
			request_id TEXT,
			event_time INTEGER,
			occurred_at INTEGER,
			org_id TEXT,
			account_id TEXT,
			seat_id TEXT,
			account_status_snapshot TEXT,
			virtual_key_id TEXT,
			virtual_key_revision TEXT,
			virtual_key_hash TEXT,
			virtual_key_alias TEXT,
			binding_id TEXT,
			credential_id TEXT,
			credential_revision TEXT,
			real_key_hash TEXT,
			credential_fingerprint TEXT,
			provider_account_fingerprint TEXT,
			provider_id TEXT,
			provider_code TEXT,
			protocol_type TEXT,
			route_source TEXT,
			model TEXT,
			request_count INTEGER DEFAULT 1,
			input_tokens INTEGER,
			output_tokens INTEGER,
			cached_input_tokens INTEGER,
			cache_creation_input_tokens INTEGER,
			reasoning_tokens INTEGER,
			total_tokens INTEGER,
			billable_amount REAL,
			currency TEXT,
			request_status TEXT,
			http_status_code INTEGER,
			upstream_request_id TEXT,
			dwd_retry_count INTEGER DEFAULT 0,
			dwd_status TEXT,
			dwd_next_retry_at INTEGER,
			dwd_last_error_code TEXT,
			dwd_last_error_msg TEXT,
			-- Phase 4 Connected Apps (v1.0.0-rc.5)
			app_slug TEXT,
			-- Performance session dimension (v1.0.0-rc.6)
			session_id TEXT,
			-- Cost-pricing audit (v1.0.0-rc.8)
			region TEXT,
			endpoint_url TEXT,
			-- OAuth identity (v1.0.1-alpha.1)
			oauth_identity TEXT,
			-- Delivery-integrity columns (rc.7 on ODS; projected to DWD in v1.0.1-alpha.3)
			content_hash TEXT,
			source_id TEXT,
			source_seq INTEGER,
			-- Full wire event (real schema: NOT NULL jsonb/TEXT). FetchPending
			-- json-extracts request_path from it (2026-07-15 非生成流量不进用量审计).
			raw_event_json TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

// insertODSTestRow inserts a minimal ODS row and returns the event_time it
// stamped — Mark* methods now take event_time (partition-pruning, v1.0.1-alpha.4)
// so callers need the value to target the row.
func insertODSTestRow(t *testing.T, db *shared.DB, odsID int64, status string, nextRetryAt aikeytime.Millis) aikeytime.Millis {
	t.Helper()
	now := aikeytime.Now()
	_, err := db.DB.Exec(`
		INSERT INTO usage_event_ods
			(ods_id, event_id, event_time, occurred_at, org_id, dwd_status, dwd_next_retry_at, request_status, request_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		odsID,
		"e-"+status,
		db.BindMillis(now),
		db.BindMillis(now),
		"org1",
		status,
		db.BindMillisPtr(func() *aikeytime.Millis {
			if nextRetryAt.IsZero() {
				return nil
			}
			return &nextRetryAt
		}()),
		"success",
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	return now
}

// TestInsertDWDFact_UpgradedSchemaNoSQLDefault is the regression guard
// for bugfix 20260424 review finding #2. Post v1.0.3-alpha the SQLite
// migration's ADD COLUMN path drops the DEFAULT expression, so an
// upgraded trial DB's `projected_at` / service-time columns have no
// SQL default. The application code must supply the value explicitly;
// this test stands up a DB whose schema mirrors the upgraded shape
// (INTEGER columns with no DEFAULT) and asserts the INSERT succeeds
// AND the written row has a non-null projected_at.
func TestInsertDWDFact_UpgradedSchemaNoSQLDefault(t *testing.T) {
	db := newSQLiteDWDTestDB(t, false /* includeSQLDefaults */)

	writer := NewSQLDWDWriter(db)
	fact := &DWDFact{
		EventID:          "test-event-1",
		RequestID:        "req-test-event-1",
		OdsID:            42,
		OccurredAt:       aikeytime.Now(),
		EventTime:        aikeytime.Now(),
		UsageDate:        "2026-04-24",
		OrgID:            "org1",
		VirtualKeyID:     "vk1",
		RequestCount:     1,
		RequestStatus:    "success",
		CompletionSource: "exact",
		QualityStatus:    QualityExact,
		BillingScope:     BillOrgAndUser,
		UserUsageScope:   UsageScopeNormal,
		ProjectorVersion: "0.1.0",
	}
	inserted, err := writer.Insert(context.Background(), fact)
	if err != nil {
		t.Fatalf("Insert against upgraded-shape schema (no SQL defaults): %v", err)
	}
	if !inserted {
		t.Fatal("Insert should have written a new row")
	}

	var projectedAt sql.NullInt64
	var requestID sql.NullString
	err = db.DB.QueryRow(
		`SELECT projected_at, request_id FROM usage_fact_dwd WHERE event_id = ?`, "test-event-1",
	).Scan(&projectedAt, &requestID)
	if err != nil {
		t.Fatalf("read projected_at: %v", err)
	}
	if !projectedAt.Valid || projectedAt.Int64 == 0 {
		t.Fatalf("projected_at must be populated from Go even when the column has no SQL DEFAULT; got Valid=%v Int64=%d", projectedAt.Valid, projectedAt.Int64)
	}
	if !requestID.Valid || requestID.String != "req-test-event-1" {
		t.Fatalf("request_id must be persisted with the fact; got %#v", requestID)
	}

	// Sanity: the value should be a recent epoch-millis, not some stray int.
	age := time.Since(aikeytime.Millis(projectedAt.Int64).Time())
	if age > 10*time.Second || age < -10*time.Second {
		t.Errorf("projected_at drift from Go Now() = %v (>10s)", age)
	}
}

// newSQLiteDWDTestDB stands up a minimal DWD table. When includeSQLDefaults
// is false, projected_at has NO DEFAULT — this mirrors the shape of a
// trial DB that went through the v1.0.3-alpha ALTER TABLE ADD COLUMN
// path (DEFAULT expressions are dropped during ADD COLUMN on affected
// SQLite bundles). When true, the column matches the baseline schema.
func newSQLiteDWDTestDB(t *testing.T, includeSQLDefaults bool) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	projectedAtCol := "projected_at INTEGER"
	if includeSQLDefaults {
		projectedAtCol = "projected_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER) * 1000)"
	}

	_, err = raw.Exec(`
		CREATE TABLE usage_fact_dwd (
			dwd_id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL,
			request_id TEXT,
			ods_id INTEGER NOT NULL,
			occurred_at INTEGER NOT NULL,
			event_time INTEGER NOT NULL,
			usage_date TEXT NOT NULL,
			org_id TEXT,
			account_id TEXT,
			seat_id TEXT,
			virtual_key_id TEXT,
			virtual_key_revision TEXT,
			virtual_key_alias TEXT,
			virtual_key_hash TEXT,
			binding_id TEXT,
			binding_alias TEXT,
			credential_id TEXT,
			credential_revision TEXT,
			real_key_hash TEXT,
			credential_fingerprint TEXT,
			provider_account_fingerprint TEXT,
			provider_id TEXT,
			provider_code TEXT,
			provider_display_name TEXT,
			protocol_type TEXT,
			route_source TEXT,
			model TEXT,
			request_count INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cached_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			billable_amount REAL,
			currency TEXT,
			request_status TEXT NOT NULL,
			http_status_code INTEGER,
			upstream_request_id TEXT,
			completion_source TEXT NOT NULL,
			quality_status TEXT NOT NULL,
			validation_code TEXT,
			validation_message TEXT,
			anomaly_type TEXT,
			anomaly_reason TEXT,
			billing_scope TEXT NOT NULL,
			user_usage_scope TEXT NOT NULL,
			control_event_id TEXT,
			control_event_revision TEXT,
			projector_version TEXT NOT NULL,
			` + projectedAtCol + `,
			-- Phase 4 Connected Apps (v1.0.0-rc.5)
			app_slug TEXT,
			-- Performance session dimension (v1.0.0-rc.6)
			session_id TEXT,
			-- Cost-pricing audit (v1.0.0-rc.8)
			region TEXT,
			endpoint_url TEXT,
			billing_period TEXT,
			unit_prices_snapshot TEXT,
			pricing_snapshot_id TEXT,
			-- OAuth identity (v1.0.1-alpha.1)
			oauth_identity TEXT,
			-- Delivery-integrity projection (v1.0.1-alpha.3)
			content_hash TEXT,
			source_id TEXT,
			source_seq INTEGER,
			UNIQUE (org_id, event_id)
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

// TestMarkDeadLetter_UpdatesStatusAndErrorFields is the regression guard
// for bugfix 20260522-projector-stuck-mark-dead-letter-param-order.
//
// Before the fix:  MarkDeadLetter bound (odsID, errCode, errMsg) into
// (dwd_last_error_code=?, dwd_last_error_msg=?, WHERE ods_id=?). Since
// ods_id is an INTEGER and errMsg is a TEXT, the WHERE never matched —
// the UPDATE affected 0 rows. dwd_status stayed at 'retry' and the
// worker re-fetched the same event every scan, stalling progress for
// every later ods_id.
//
// After the fix:  bind order is (errCode, errMsg, odsID), so the row
// transitions to dwd_status='dead_letter' and the worker advances past
// it on the next FetchPending.
func TestMarkDeadLetter_UpdatesStatusAndErrorFields(t *testing.T) {
	db := newSQLiteODSTestDB(t)
	eventTime := insertODSTestRow(t, db, 42, "retry", aikeytime.Millis(0))

	// Sanity precondition: row exists in 'retry' state with no error fields.
	var preStatus string
	if err := db.DB.QueryRow(
		`SELECT dwd_status FROM usage_event_ods WHERE ods_id = ?`, 42,
	).Scan(&preStatus); err != nil {
		t.Fatalf("pre-check select: %v", err)
	}
	if preStatus != "retry" {
		t.Fatalf("pre-check: expected dwd_status='retry', got %q", preStatus)
	}

	reader := NewSQLODSReader(db)
	if err := reader.MarkDeadLetter(context.Background(), 42, eventTime, "ENRICH_FAILED", "schema mismatch"); err != nil {
		t.Fatalf("MarkDeadLetter: %v", err)
	}

	// Post-check: the row must now be 'dead_letter' AND carry the
	// error fields we passed. The pre-fix bug would leave dwd_status
	// at 'retry' (UPDATE 0 rows) — that single assertion catches it.
	var (
		gotStatus    string
		gotErrCode   sql.NullString
		gotErrMsg    sql.NullString
	)
	if err := db.DB.QueryRow(
		`SELECT dwd_status, dwd_last_error_code, dwd_last_error_msg
		 FROM usage_event_ods WHERE ods_id = ?`, 42,
	).Scan(&gotStatus, &gotErrCode, &gotErrMsg); err != nil {
		t.Fatalf("post-check select: %v", err)
	}

	if gotStatus != "dead_letter" {
		t.Errorf("dwd_status: want 'dead_letter', got %q — MarkDeadLetter UPDATE missed the row (likely parameter-order regression)", gotStatus)
	}
	if !gotErrCode.Valid || gotErrCode.String != "ENRICH_FAILED" {
		t.Errorf("dwd_last_error_code: want 'ENRICH_FAILED', got %#v — parameter binding swapped errCode with something else", gotErrCode)
	}
	if !gotErrMsg.Valid || gotErrMsg.String != "schema mismatch" {
		t.Errorf("dwd_last_error_msg: want 'schema mismatch', got %#v — parameter binding swapped errMsg with something else", gotErrMsg)
	}
}

// TestFetchPending_RequestPathFromRawEvent pins the 2026-07-15 非生成流量
// contract on the read side: FetchPending must surface the wire event's
// request_path out of raw_event_json (SQL-side json extraction — no ODS
// column), and must scan NULL cleanly for legacy rows whose raw event
// predates the field (or whose raw_event_json is NULL in this fixture).
func TestFetchPending_RequestPathFromRawEvent(t *testing.T) {
	db := newSQLiteODSTestDB(t)

	// Row 1: modern event carrying request_path inside the raw wire JSON.
	et := insertODSTestRow(t, db, 1, "pending", aikeytime.Millis(0))
	if _, err := db.DB.Exec(
		`UPDATE usage_event_ods SET raw_event_json = ? WHERE ods_id = 1`,
		`{"event_id":"e-pending","request_path":"/openai/v1/models"}`,
	); err != nil {
		t.Fatalf("seed raw_event_json: %v", err)
	}
	_ = et
	// Row 2: legacy event — raw_event_json has no request_path key.
	insertODSTestRow(t, db, 2, "pending", aikeytime.Millis(0))
	if _, err := db.DB.Exec(
		`UPDATE usage_event_ods SET raw_event_json = ? WHERE ods_id = 2`,
		`{"event_id":"e-legacy"}`,
	); err != nil {
		t.Fatalf("seed legacy raw_event_json: %v", err)
	}

	reader := NewSQLODSReader(db)
	pending, err := reader.FetchPending(context.Background(), 100)
	if err != nil {
		t.Fatalf("FetchPending: %v", err)
	}
	byID := map[int64]ODSRecord{}
	for _, p := range pending {
		byID[p.OdsID] = p
	}
	modern, ok := byID[1]
	if !ok {
		t.Fatal("row 1 missing from FetchPending")
	}
	if !modern.RequestPath.Valid || modern.RequestPath.String != "/openai/v1/models" {
		t.Errorf("RequestPath = %+v, want valid /openai/v1/models", modern.RequestPath)
	}
	legacy, ok := byID[2]
	if !ok {
		t.Fatal("row 2 missing from FetchPending")
	}
	if legacy.RequestPath.Valid {
		t.Errorf("legacy RequestPath = %+v, want NULL", legacy.RequestPath)
	}
}
