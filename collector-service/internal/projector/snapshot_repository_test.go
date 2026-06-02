package projector

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// newSQLiteSnapshotTestDB builds an in-memory DB with the pricing_snapshots
// table. Schema mirrors v1_0_0_rc8_pricing.go UpgradeSQLite — keep in sync.
func newSQLiteSnapshotTestDB(t *testing.T) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err = raw.Exec(`CREATE TABLE pricing_snapshots (
		snapshot_id TEXT NOT NULL,
		litellm_sha256 TEXT NOT NULL,
		history_sha256 TEXT NOT NULL,
		overrides_sha256 TEXT NOT NULL,
		aikey_version TEXT,
		created_at INTEGER NOT NULL,
		effective_from INTEGER NOT NULL,
		effective_until INTEGER,
		notes TEXT,
		PRIMARY KEY (snapshot_id)
	)`); err != nil {
		t.Fatal(err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

func snapRowCount(t *testing.T, db *shared.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pricing_snapshots`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func snapActiveID(t *testing.T, db *shared.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(context.Background(),
		`SELECT snapshot_id FROM pricing_snapshots WHERE effective_until IS NULL`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// EnsureSnapshot: insert-if-new, repeat is a no-op, a new snapshot supersedes
// the prior active one (which gets effective_until), and rows are never deleted.
func TestEnsureSnapshot_InsertRepeatSupersede(t *testing.T) {
	db := newSQLiteSnapshotTestDB(t)
	repo := NewSQLSnapshotRepository(db)
	ctx := context.Background()

	snapA := pricing.Snapshot{SnapshotID: "aaaa", LiteLLMSHA256: "l1", HistorySHA256: "h1", OverridesSHA256: "o1"}
	if err := repo.EnsureSnapshot(ctx, snapA, "v1", 1000); err != nil {
		t.Fatal(err)
	}
	if snapRowCount(t, db) != 1 || snapActiveID(t, db) != "aaaa" {
		t.Fatalf("after first insert: rows=%d active=%s", snapRowCount(t, db), snapActiveID(t, db))
	}

	// Repeat same content → no-op (still 1 row, still active aaaa).
	if err := repo.EnsureSnapshot(ctx, snapA, "v1", 1100); err != nil {
		t.Fatal(err)
	}
	if snapRowCount(t, db) != 1 {
		t.Errorf("repeat must be no-op, rows=%d want 1", snapRowCount(t, db))
	}

	// New snapshot → 2 rows, new is active, old is superseded with effective_until.
	snapB := pricing.Snapshot{SnapshotID: "bbbb", LiteLLMSHA256: "l2", HistorySHA256: "h1", OverridesSHA256: "o1"}
	if err := repo.EnsureSnapshot(ctx, snapB, "v2", 2000); err != nil {
		t.Fatal(err)
	}
	if snapRowCount(t, db) != 2 {
		t.Errorf("new snapshot must add a row (never delete), rows=%d want 2", snapRowCount(t, db))
	}
	if snapActiveID(t, db) != "bbbb" {
		t.Errorf("active = %s, want bbbb", snapActiveID(t, db))
	}
	var eu sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT effective_until FROM pricing_snapshots WHERE snapshot_id=?`, "aaaa").Scan(&eu); err != nil {
		t.Fatal(err)
	}
	if !eu.Valid || eu.Int64 != 2000 {
		t.Errorf("old snapshot effective_until = %v, want 2000", eu)
	}
}
