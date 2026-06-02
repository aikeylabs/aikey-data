package projector

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// newSQLiteUnpricedTestDB builds an in-memory DB with the unpriced_models table.
// Schema mirrors v1_0_0_rc8_pricing.go UpgradeSQLite — keep in sync.
func newSQLiteUnpricedTestDB(t *testing.T) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err = raw.Exec(`CREATE TABLE unpriced_models (
		model TEXT NOT NULL,
		provider TEXT NOT NULL,
		first_seen_at INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		event_count INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending',
		notes TEXT,
		PRIMARY KEY (model, provider)
	)`); err != nil {
		t.Fatal(err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

// First sighting inserts; repeats bump event_count + last_seen_at; distinct
// (model, provider) is a separate row.
func TestUpsertUnpriced_InsertThenBump(t *testing.T) {
	db := newSQLiteUnpricedTestDB(t)
	repo := NewSQLUnpricedRepository(db)
	ctx := context.Background()

	if err := repo.UpsertUnpriced(ctx, "anthropic", "claude-x", 1000); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUnpriced(ctx, "anthropic", "claude-x", 2000); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUnpriced(ctx, "openai", "gpt-z", 1500); err != nil {
		t.Fatal(err)
	}

	var count, lastSeen, firstSeen int64
	if err := db.QueryRowContext(ctx,
		`SELECT event_count, last_seen_at, first_seen_at FROM unpriced_models WHERE model=? AND provider=?`,
		"claude-x", "anthropic").Scan(&count, &lastSeen, &firstSeen); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("event_count = %d, want 2", count)
	}
	if lastSeen != 2000 {
		t.Errorf("last_seen_at = %d, want 2000 (bumped)", lastSeen)
	}
	if firstSeen != 1000 {
		t.Errorf("first_seen_at = %d, want 1000 (unchanged)", firstSeen)
	}

	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM unpriced_models`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("rows = %d, want 2 (claude-x + gpt-z are distinct)", rows)
	}
}
