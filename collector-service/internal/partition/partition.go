// Package partition manages monthly RANGE partitions for the high-volume
// usage tables on PostgreSQL (usage_fact_dwd by usage_date, usage_event_ods by
// event_time). Enterprise usage-audit, v1.0.1-alpha.4.
//
// Why a runtime helper and not pure baseline DDL: monthly partition bounds are
// time-dependent ("2026-06" only exists once 2026-06 is near), so they can't be
// hardcoded in the frozen baseline. The baseline creates the partitioned parent
// + a DEFAULT partition (so inserts never fail); this helper, run at collector
// startup, pre-creates the current and next months so live writes land in a
// dedicated partition that the audit queries can prune to.
//
// SQLite (Personal/Trial) is not partitioned — every function here is a no-op
// off PostgreSQL.
package partition

import (
	"context"
	"fmt"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// Monthly key kinds — controls the bound literal so a DATE key and a TIMESTAMPTZ
// key both get a well-typed FROM/TO.
const (
	KeyDate        = "date"        // usage_fact_dwd.usage_date  (DATE)
	KeyTimestamptz = "timestamptz" // usage_event_ods.event_time (TIMESTAMPTZ)
)

// EnsureMonthlyPartitions creates the current month plus `ahead` following
// months as RANGE partitions of `table`. PostgreSQL only — a no-op on SQLite.
// Idempotent (CREATE TABLE IF NOT EXISTS), so it is safe to call on every boot.
//
// `now` is injected (not time.Now() internally) so callers/tests stay
// deterministic. `ahead` ≥ 1 gives slack: a collector that runs longer than a
// month without restart still has the next month's partition ready, so rows
// don't fall back to DEFAULT (which would then block that month's partition
// creation on the following restart — the PG "default partition has conflicting
// rows" gotcha).
func EnsureMonthlyPartitions(ctx context.Context, db *shared.DB, table, keyKind string, ahead int, now time.Time) error {
	if db.Dialect != shared.DialectPostgres {
		return nil
	}
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= ahead; i++ {
		from := start.AddDate(0, i, 0)
		to := from.AddDate(0, 1, 0)
		name := fmt.Sprintf("%s_%04d_%02d", table, from.Year(), int(from.Month()))
		stmt := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
			name, table, monthBound(keyKind, from), monthBound(keyKind, to))
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create partition %s: %w", name, err)
		}
	}
	return nil
}

func monthBound(keyKind string, t time.Time) string {
	if keyKind == KeyTimestamptz {
		return t.Format("2006-01-02 15:04:05-07:00")
	}
	return t.Format("2006-01-02")
}
