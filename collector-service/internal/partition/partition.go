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
	"strconv"
	"strings"
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

// DropPartitionsBefore drops every monthly partition of `table` whose month is
// entirely older than `cutoff` (the start of the retention window). PostgreSQL
// only — a no-op on SQLite (not partitioned; Cluster conversation-audit runs on
// PG). The DEFAULT partition and any non-monthly child are never touched (only
// children named `<table>_YYYY_MM`). Returns the dropped partition names for
// logging. Idempotent: a partition already gone is simply absent from the list.
func DropPartitionsBefore(ctx context.Context, db *shared.DB, table string, cutoff time.Time) ([]string, error) {
	if db.Dialect != shared.DialectPostgres {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname
		  FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = ?`, table)
	if err != nil {
		return nil, fmt.Errorf("list partitions of %s: %w", table, err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan partition name: %w", err)
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	toDrop := partitionsToDropBefore(names, table, cutoff)
	for _, n := range toDrop {
		// n is a real child-partition relname matching <table>_YYYY_MM (validated
		// in partitionsToDropBefore), never user input — safe to interpolate.
		if _, err := db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", n)); err != nil {
			return toDrop, fmt.Errorf("drop partition %s: %w", n, err)
		}
	}
	return toDrop, nil
}

// partitionsToDropBefore is the pure decision core of DropPartitionsBefore (DB-
// free, fully testable): from a parent's child names, return those monthly
// partitions whose month starts strictly before cutoff's month. Non-monthly
// children (e.g. `<table>_default`) are skipped.
func partitionsToDropBefore(names []string, table string, cutoff time.Time) []string {
	cutoffMonth := time.Date(cutoff.Year(), cutoff.Month(), 1, 0, 0, 0, 0, time.UTC)
	prefix := table + "_"
	var out []string
	for _, n := range names {
		y, m, ok := parseMonthSuffix(n, prefix)
		if !ok {
			continue // DEFAULT partition / non-monthly child — never drop
		}
		monthStart := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
		if monthStart.Before(cutoffMonth) {
			out = append(out, n)
		}
	}
	return out
}

// parseMonthSuffix extracts (year, month) from a `<prefix>YYYY_MM` partition
// name. ok=false for anything else (the DEFAULT partition, foreign children).
func parseMonthSuffix(name, prefix string) (year, month int, ok bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, 0, false
	}
	parts := strings.Split(name[len(prefix):], "_")
	if len(parts) != 2 || len(parts[0]) != 4 || len(parts[1]) != 2 {
		return 0, 0, false
	}
	y, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || m < 1 || m > 12 {
		return 0, 0, false
	}
	return y, m, true
}
