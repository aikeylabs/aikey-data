package partition

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	_ "modernc.org/sqlite"
)

// TestEnsureMonthlyPartitions_SQLiteNoOp pins that partitioning is PostgreSQL-
// only: on SQLite (Personal/Trial) the helper must be a clean no-op and create
// nothing — the single-table baseline stays intact.
func TestEnsureMonthlyPartitions_SQLiteNoOp(t *testing.T) {
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := shared.NewDB(raw, shared.DialectSQLite)

	if err := EnsureMonthlyPartitions(context.Background(), db, "usage_fact_dwd", KeyDate, 2, time.Now()); err != nil {
		t.Fatalf("SQLite path must be a no-op, got error: %v", err)
	}
	// No partition tables should have been created.
	var n int
	_ = raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE 'usage_fact_dwd_%'`).Scan(&n)
	if n != 0 {
		t.Errorf("SQLite must not create partition tables, found %d", n)
	}
}

// TestMonthBound pins the per-key-kind bound literal so a DATE partition key and
// a TIMESTAMPTZ partition key both get a well-typed FROM/TO.
func TestMonthBound(t *testing.T) {
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if got := monthBound(KeyDate, at); got != "2026-06-01" {
		t.Errorf("date bound: want 2026-06-01, got %q", got)
	}
	if got := monthBound(KeyTimestamptz, at); got != "2026-06-01 00:00:00+00:00" {
		t.Errorf("timestamptz bound: want 2026-06-01 00:00:00+00:00, got %q", got)
	}
}
