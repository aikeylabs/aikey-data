package shared

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

const (
	DialectPostgres = "postgres"
	DialectSQLite   = "sqlite"
)

// DB wraps *sql.DB with dialect awareness. Repository code uses ? placeholders
// universally; the wrapper rewrites them to $1,$2,... for PostgreSQL.
type DB struct {
	*sql.DB
	Dialect string
}

func NewDB(db *sql.DB, dialect string) *DB { return &DB{DB: db, Dialect: dialect} }

func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.DB.ExecContext(ctx, d.rewrite(query), args...)
}
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, d.rewrite(query), args...)
}
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.DB.QueryRowContext(ctx, d.rewrite(query), args...)
}

func (d *DB) InsertOrIgnore(table, columns, placeholders string) string {
	if d.Dialect == DialectSQLite {
		return fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)", table, columns, placeholders)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING", table, columns, placeholders)
}

// InsertOrIgnoreOn returns INSERT...ON CONFLICT (cols) DO NOTHING for PG,
// or INSERT OR IGNORE for SQLite.
func (d *DB) InsertOrIgnoreOn(table, columns, placeholders, conflictCols string) string {
	if d.Dialect == DialectSQLite {
		return fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)", table, columns, placeholders)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO NOTHING", table, columns, placeholders, conflictCols)
}

func (d *DB) Now() string {
	if d.Dialect == DialectSQLite {
		return "datetime('now')"
	}
	return "NOW()"
}

func (d *DB) IsSQLite() bool { return d.Dialect == DialectSQLite }

// HourBucket returns a dialect-specific SQL expression that extracts
// the UTC hour (0..23 as INTEGER) from a timestamp column. Intended
// for use inside SELECT lists or GROUP BY clauses where the produced
// expression is interpolated via fmt.Sprintf.
//
// Storage convention after v1.0.3-alpha (β-hybrid):
//   - SQLite column is INTEGER holding Unix epoch milliseconds (UTC).
//     Extract hour via strftime on the seconds-resolution view of the
//     millis, with the 'unixepoch' modifier so SQLite interprets the
//     value as epoch rather than an ISO string.
//   - Postgres column is TIMESTAMPTZ. Extract UTC hour as before.
//
// Why we switched from strftime on the raw text: the text path broke
// because Go's default time.Time String() format ("2026-04-24
// 14:30:00 +0800 CST") is not recognised by strftime — the query
// returned NULL and the endpoint 500'd. Design doc:
// roadmap20260320/技术实现/update/20260424-时间戳统一为int64毫秒-data-service.md
func (d *DB) HourBucket(col string) string {
	if d.Dialect == DialectSQLite {
		// col / 1000 truncates to whole seconds; 'unixepoch' tells
		// strftime to treat the integer as Unix seconds (UTC).
		return fmt.Sprintf("CAST(strftime('%%H', %s / 1000, 'unixepoch') AS INTEGER)", col)
	}
	return fmt.Sprintf("CAST(EXTRACT(HOUR FROM %s AT TIME ZONE 'UTC') AS INTEGER)", col)
}

// BindMillis and BindMillisPtr mirror the collector-service helpers
// (see aikey-data/collector-service/internal/shared/dbkit.go) so
// query-service write / filter sites can bind aikeytime.Millis values
// without every caller branching on dialect. The query-service is
// read-mostly but still binds startDate / endDate filters on millis
// columns, so this helper centralises the β-hybrid bind policy.
func (d *DB) BindMillis(m aikeytime.Millis) any {
	if m.IsZero() {
		return nil
	}
	if d.Dialect == DialectSQLite {
		return m.Int64()
	}
	return m.Time()
}

func (d *DB) BindMillisPtr(m *aikeytime.Millis) any {
	if m == nil || m.IsZero() {
		return nil
	}
	if d.Dialect == DialectSQLite {
		return m.Int64()
	}
	return m.Time()
}

// DateString returns a dialect-specific SELECT expression that
// projects a DATE-typed column as YYYY-MM-DD text. In Postgres a
// cast to `::text` is required; in SQLite the column is already
// stored as TEXT (the schema uses TEXT for usage_date), so the
// column name is returned verbatim.
//
// Prior to 2026-04-24, repository code hard-coded `usage_date::text`
// and relied on the dbkit rewrite step stripping `::text` on SQLite
// (stripPgCasts). That works, but leaves an implicit dependency on
// regex-level rewriting; making the dialect choice explicit through
// this helper removes the magic.
func (d *DB) DateString(col string) string {
	if d.Dialect == DialectSQLite {
		return col
	}
	return col + "::text"
}

// DateOf returns a SQL expression that projects a timestamp column
// (INTEGER millis on SQLite, TIMESTAMPTZ on Postgres) as a DATE.
//
//   - SQLite: DATE(col/1000, 'unixepoch') — 'unixepoch' tells SQLite
//     to treat the int as seconds since epoch UTC. Without it, the
//     legacy DATE(event_time) returned NULL on INTEGER input and
//     silently dropped rows.
//   - Postgres: DATE(col AT TIME ZONE 'UTC') — explicit UTC so
//     intra-day boundary behavior matches SQLite.
//
// See bugfix 20260424 review finding #3.
func (d *DB) DateOf(col string) string {
	if d.Dialect == DialectSQLite {
		return fmt.Sprintf("DATE(%s/1000, 'unixepoch')", col)
	}
	return fmt.Sprintf("DATE(%s AT TIME ZONE 'UTC')", col)
}

// HourBucketLocal is the tz-aware counterpart to HourBucket. Shifts the
// int64 millis column by `offsetMs` before extracting the hour, so the
// result is the hour in the user's local timezone.
//
//   - offsetMs = 0          → same as HourBucket (UTC hour).
//   - offsetMs = 8*3600_000 → +08:00 (Asia/Shanghai); UTC 04:00 → local 12.
//
// DST caveat: the offset is constant across the SQL expression, so a
// query window that straddles a DST transition will mis-bucket
// exactly one hour on the transition day. The caller
// (QueryParams.Defaults) computes the offset at the query **window's
// start** — not at `time.Now()` — so a historical query from a DST
// region picks the offset that applied during the window, not the
// caller's current-season offset. This keeps the error bounded to
// the ≤1 DST-crossing day per window rather than the entire historic
// window.
//
// Postgres takes the tz *name* (IANA) rather than a raw offset so DST
// is respected inside the query itself (per-row).
func (d *DB) HourBucketLocal(col string, offsetMs int64, tzName string) string {
	if d.Dialect == DialectSQLite {
		// ((col + offset) / 3_600_000) %% 24 — pure integer math, no
		// strftime / unixepoch roundtrip, same correctness as UTC path.
		return fmt.Sprintf("((%s + %d) / 3600000) %% 24", col, offsetMs)
	}
	// Postgres: EXTRACT(HOUR FROM col AT TIME ZONE '<iana>') yields the
	// hour in that named zone, DST-aware per row.
	return fmt.Sprintf("CAST(EXTRACT(HOUR FROM %s AT TIME ZONE %s) AS INTEGER)", col, pgLiteral(tzName))
}

// DateOfLocal is the tz-aware counterpart to DateOf. Projects the
// millis column as a local YYYY-MM-DD string.
func (d *DB) DateOfLocal(col string, offsetMs int64, tzName string) string {
	if d.Dialect == DialectSQLite {
		return fmt.Sprintf("DATE((%s + %d)/1000, 'unixepoch')", col, offsetMs)
	}
	return fmt.Sprintf("DATE(%s AT TIME ZONE %s)", col, pgLiteral(tzName))
}

// pgLiteral wraps a string in single quotes with minimal escaping for
// inline interpolation into a Postgres SQL fragment. Only used for
// IANA tz names (controlled whitelist at the API boundary).
func pgLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (d *DB) rewrite(query string) string {
	// SQLite: strip PostgreSQL-style type casts (e.g. ::text, ::integer).
	// In SQLite columns are already dynamically typed so the cast is a no-op.
	if d.Dialect == DialectSQLite {
		query = stripPgCasts(query)
	}
	if d.Dialect != DialectPostgres || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	n := 1
	inStr := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			inStr = !inStr
		}
		if ch == '?' && !inStr {
			b.WriteByte('$')
			fmt.Fprintf(&b, "%d", n)
			n++
		} else {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// pgCastRe matches PostgreSQL-style type casts like ::text, ::integer, ::bigint.
var pgCastRe = regexp.MustCompile(`::[a-zA-Z]+`)

// stripPgCasts removes PostgreSQL :: type casts which are unsupported in SQLite.
// SQLite uses dynamic typing so the casts are unnecessary.
func stripPgCasts(query string) string {
	return pgCastRe.ReplaceAllString(query, "")
}
