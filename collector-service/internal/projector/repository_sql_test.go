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
			event_time INTEGER,
			occurred_at INTEGER,
			org_id TEXT,
			account_id TEXT,
			seat_id TEXT,
			account_status_snapshot TEXT,
			virtual_key_id TEXT,
			virtual_key_revision TEXT,
			virtual_key_hash TEXT,
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
			reasoning_tokens INTEGER,
			total_tokens INTEGER,
			billable_amount REAL,
			currency TEXT,
			request_status TEXT,
			http_status_code INTEGER,
			upstream_request_id TEXT,
			dwd_retry_count INTEGER DEFAULT 0,
			dwd_status TEXT,
			dwd_next_retry_at INTEGER
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

func insertODSTestRow(t *testing.T, db *shared.DB, odsID int64, status string, nextRetryAt aikeytime.Millis) {
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
	err = db.DB.QueryRow(
		`SELECT projected_at FROM usage_fact_dwd WHERE event_id = ?`, "test-event-1",
	).Scan(&projectedAt)
	if err != nil {
		t.Fatalf("read projected_at: %v", err)
	}
	if !projectedAt.Valid || projectedAt.Int64 == 0 {
		t.Fatalf("projected_at must be populated from Go even when the column has no SQL DEFAULT; got Valid=%v Int64=%d", projectedAt.Valid, projectedAt.Int64)
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
			UNIQUE (org_id, event_id)
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}
