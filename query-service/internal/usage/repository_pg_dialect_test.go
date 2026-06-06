package usage

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

// TestPersonalQueries_PostgresDialect is the dialect regression guard for the
// 2026-06-05 by-key 500 bug
// (workflow/CI/bugfix/2026-06-05-team-by-key-postgres-group-by-500.md).
//
// Why this exists: the rest of the usage suite runs only against SQLite
// in-memory, and SQLite SILENTLY tolerates a non-grouped, non-aggregated SELECT
// column. PostgreSQL rejects it ("column X must appear in the GROUP BY clause").
// That gap is exactly how `PersonalByKeyTotal` shipped a query that 500'd on
// every Production/Trial-on-PG deployment while the Personal (SQLite) page
// looked fine. This test re-runs every personal usage query against a real
// PostgreSQL so a future GROUP BY / aggregate slip is caught at review time.
//
// Run it against a MIGRATED PostgreSQL (e.g. the Production stack's PG):
//
//	TEST_USAGE_PG_DSN='postgres://aikey:<pw>@127.0.0.1:5432/aikey_control?sslmode=disable' \
//	  go test ./internal/usage/ -run PostgresDialect
//
// Skipped when the env var is unset, so the default SQLite-only suite still
// runs everywhere. Dialect violations (GROUP BY / aggregates) error at query
// PLAN time, independent of data, so a dummy account_id with zero rows is
// enough — no seed data required.
func TestPersonalQueries_PostgresDialect(t *testing.T) {
	dsn := os.Getenv("TEST_USAGE_PG_DSN")
	if dsn == "" {
		t.Skip("set TEST_USAGE_PG_DSN to a migrated PostgreSQL DSN to run the dialect regression")
	}

	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.Ping(); err != nil {
		t.Fatalf("ping pg (is TEST_USAGE_PG_DSN reachable + migrated?): %v", err)
	}

	repo := NewSQLRepository(shared.NewDB(raw, shared.DialectPostgres))
	ctx := context.Background()
	// No rows for this account; the plan-time GROUP BY / aggregate check is what
	// we're after, and it fires regardless of result-set size.
	p := QueryParams{
		AccountID: "00000000-0000-0000-0000-000000000000",
		StartDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EndDate:   time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		Limit:     50,
	}

	// Every personal usage query must PLAN + EXECUTE on PostgreSQL without a
	// dialect error. Listed explicitly so a newly-added query that forgets the
	// GROUP BY / aggregate rules trips this guard.
	checks := []struct {
		name string
		run  func() error
	}{
		{"PersonalByKeyTotal", func() error { _, e := repo.PersonalByKeyTotal(ctx, p); return e }},
		{"PersonalByAppTotal", func() error { _, e := repo.PersonalByAppTotal(ctx, p); return e }},
		{"PersonalByProtocolTotal", func() error { _, e := repo.PersonalByProtocolTotal(ctx, p); return e }},
		{"PersonalByModelTotal", func() error { _, e := repo.PersonalByModelTotal(ctx, p); return e }},
		{"PersonalBySessionTotal", func() error { _, e := repo.PersonalBySessionTotal(ctx, p); return e }},
		{"PersonalTimeline", func() error { _, e := repo.PersonalTimeline(ctx, p); return e }},
		{"PersonalHourlyTimeline", func() error { _, e := repo.PersonalHourlyTimeline(ctx, p); return e }},
		{"PersonalByProtocolTimeline", func() error { _, e := repo.PersonalByProtocolTimeline(ctx, p); return e }},
		{"PersonalByProtocolHourly", func() error { _, e := repo.PersonalByProtocolHourly(ctx, p); return e }},
		// 2026-06-05: per-row event_time must project to int64 millis (EpochMillis)
		// or the Scan fails on PG ("converting time.Time to int64"). NOTE:
		// PersonalRecent has the same per-row event_time Scan and is NOT yet
		// guarded here — it has the identical latent PG bug (only ever run on
		// SQLite Personal so far).
		{"PersonalUsageDetail", func() error { _, e := repo.PersonalUsageDetail(ctx, p); return e }},
	}
	for _, c := range checks {
		if err := c.run(); err != nil {
			t.Errorf("%s failed on PostgreSQL (dialect regression): %v", c.name, err)
		}
	}
}
