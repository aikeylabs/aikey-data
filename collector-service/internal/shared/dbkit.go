package shared

import (
	"context"
	"database/sql"
	"fmt"
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

// NowMillis returns a SQL expression that evaluates to "the current
// moment, in the same representation as int64-millis / TIMESTAMPTZ
// timestamp columns". Use this when generating a comparison against
// a timestamp column populated by the v1.0.3-alpha β-hybrid scheme
// (SQLite INTEGER millis, Postgres TIMESTAMPTZ).
//
// Why a separate helper from Now(): the legacy Now() returns
// datetime('now') on SQLite — a TEXT value that compares
// lexicographically against INTEGER millis and always yields TRUE
// (string "1777041000000" < string "2026-04-24 05:13:34"), causing
// retry rows to hot-loop instead of waiting for their scheduled
// retry time. See bugfix 20260424.
func (d *DB) NowMillis() string {
	if d.Dialect == DialectSQLite {
		// (strftime('%s','now') * 1000) — UTC seconds × 1000 = UTC millis.
		// CAST to INTEGER is belt-and-braces: strftime returns TEXT,
		// and we want a clean INTEGER for comparison affinity.
		return "(CAST(strftime('%s','now') AS INTEGER) * 1000)"
	}
	// Postgres: dwd_next_retry_at / effective_from etc. remain TIMESTAMPTZ
	// under β-hybrid. NOW() returns TIMESTAMPTZ, native comparison works.
	return "NOW()"
}

// NowEpochMillis returns a SQL expression that evaluates to the current
// moment as int64 epoch-millis on BOTH dialects (SQLite INTEGER, Postgres
// BIGINT). Use this for plain-bigint timestamp columns — e.g. the quota_*
// tables (quota_subject / quota_counter), whose updated_at/created_at are
// declared BIGINT (PG) / INTEGER (SQLite) in v1_0_0_rc7_quota.go, NOT the
// β-hybrid TIMESTAMPTZ shape.
//
// Why a separate helper from NowMillis(): NowMillis() returns NOW()
// (TIMESTAMPTZ) on Postgres because it targets β-hybrid columns. Feeding
// that into a BIGINT column raises `column "updated_at" is of type bigint
// but expression is of type timestamp with time zone` — the materializer
// UPSERT silently failed on Postgres while SQLite (loose typing) passed.
// See workflow/CI/bugfix/2026-06-03-team-usage-refresh-contract-mismatch.md.
func (d *DB) NowEpochMillis() string {
	if d.Dialect == DialectSQLite {
		return "(CAST(strftime('%s','now') AS INTEGER) * 1000)"
	}
	// Postgres: EXTRACT(EPOCH ...) is double seconds; ×1000 → millis; cast
	// to bigint to match the column type (mirrors AgeMillis's PG branch).
	return "(EXTRACT(EPOCH FROM NOW()) * 1000)::bigint"
}

// DateOf returns a SQL expression that projects a timestamp column
// as a DATE / YYYY-MM-DD string. Used for grouping / filtering that
// needs the calendar day of an instant.
//
//   - SQLite (INTEGER millis): DATE(col/1000, 'unixepoch')
//   - Postgres (TIMESTAMPTZ) : DATE(col AT TIME ZONE 'UTC')
//
// Why a helper: SQLite's DATE() returns NULL when handed an INTEGER
// without the 'unixepoch' modifier — the legacy DATE(event_time)
// compiled fine but silently dropped every row (see bugfix
// 20260424 review finding #3).
func (d *DB) DateOf(col string) string {
	if d.Dialect == DialectSQLite {
		return fmt.Sprintf("DATE(%s/1000, 'unixepoch')", col)
	}
	return fmt.Sprintf("DATE(%s AT TIME ZONE 'UTC')", col)
}

// AgeMillis returns a SQL expression evaluating to "how many milliseconds ago
// the instant in `col` was", as an INTEGER, on either dialect. Use it to
// age-gate timestamp columns (e.g. usage_source_watermark.updated_at /
// last_event_at) so Go can compare the returned age against a duration
// threshold without parsing dialect-specific timestamp text.
//
//   - SQLite (INTEGER millis): now_millis - col
//   - Postgres (TIMESTAMPTZ) : EXTRACT(EPOCH FROM (NOW() - col)) * 1000, cast to bigint
//
// Why a helper (mirrors NowMillis / DateOf): "now - col" is millis-subtraction
// on SQLite but an INTERVAL on Postgres — a single literal would be wrong on
// one dialect. A NULL col yields NULL (a never-seen column ages to "unknown",
// which callers treat as not-stale, not as an infinite gap).
func (d *DB) AgeMillis(col string) string {
	if d.Dialect == DialectSQLite {
		return fmt.Sprintf("(CAST(strftime('%%s','now') AS INTEGER) * 1000 - %s)", col)
	}
	return fmt.Sprintf("(EXTRACT(EPOCH FROM (NOW() - %s)) * 1000)::bigint", col)
}

// Greatest returns a SQL expression for the two-argument maximum, dialect-safe.
// SQLite's MAX(a, b) is a SCALAR function; on Postgres MAX is an AGGREGATE only —
// its two-argument scalar max is GREATEST(a, b). Use this for monotonic UPSERT
// guards (never-regress columns) so they run on BOTH backends.
//
// Why a helper: a literal `MAX(a, b)` compiles + works on SQLite but throws
// `function max(bigint, bigint) does not exist` on Postgres — a dialect bug that
// only surfaces on a 2nd-batch watermark UPSERT (ON CONFLICT DO UPDATE), which
// SQLite-only tests never catch.
func (d *DB) Greatest(a, b string) string {
	if d.Dialect == DialectSQLite {
		return fmt.Sprintf("MAX(%s, %s)", a, b)
	}
	return fmt.Sprintf("GREATEST(%s, %s)", a, b)
}

// JSONText returns a SQL expression extracting a top-level string key from a
// JSON column as TEXT (SQL NULL when the key is absent), dialect-safe.
// SQLite stores JSON as TEXT and uses json_extract(col,'$.key'); Postgres
// stores JSONB and uses col->>'key'. key must be a trusted literal (compile-time
// constant), never user input — it is interpolated, not bound.
//
// Why a helper (2026-07-15 非生成流量不进用量审计): the projector reads the wire
// event's request_path out of usage_event_ods.raw_event_json instead of adding
// an ODS column — additive wire fields then need zero DDL. A literal `->>`
// works on PG but not SQLite; json_extract works on SQLite but not on JSONB
// the same way — same class of dialect bug as Greatest above.
func (d *DB) JSONText(col, key string) string {
	if d.Dialect == DialectSQLite {
		return fmt.Sprintf("json_extract(%s, '$.%s')", col, key)
	}
	return fmt.Sprintf("%s->>'%s'", col, key)
}

func (d *DB) IsSQLite() bool { return d.Dialect == DialectSQLite }

// BindMillis returns the correct driver argument for an aikeytime.Millis
// value on the current dialect. β-hybrid:
//
//   - SQLite → INTEGER column: emit int64 millis. Zero → SQL NULL so
//     NOT-NULL columns that receive a zero value still error loudly
//     (the caller must set the time before insert).
//   - Postgres → TIMESTAMPTZ column: emit time.Time (UTC). Zero → NULL.
//
// Write sites call this at every bind so the wrapper type never
// reaches the driver's generic Valuer path — which would emit int64
// regardless and break TIMESTAMPTZ columns on Postgres.
func (d *DB) BindMillis(m aikeytime.Millis) any {
	if m.IsZero() {
		return nil
	}
	if d.Dialect == DialectSQLite {
		return m.Int64()
	}
	return m.Time()
}

// BindMillisPtr is the nullable-pointer variant. nil → NULL.
func (d *DB) BindMillisPtr(m *aikeytime.Millis) any {
	if m == nil || m.IsZero() {
		return nil
	}
	if d.Dialect == DialectSQLite {
		return m.Int64()
	}
	return m.Time()
}

func (d *DB) rewrite(query string) string {
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
