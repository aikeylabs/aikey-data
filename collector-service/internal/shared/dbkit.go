package shared

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/AiKeyLabs/pkg/aikeytime"
	"github.com/lib/pq"
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

// Execer is the minimal statement-execution surface shared by *DB (autocommit)
// and *Tx (batch transaction). Repository write paths that must be able to run
// inside a batch transaction execute through an Execer instead of *DB directly;
// dialect helpers (pure string/bind builders) stay on *DB. Introduced for the
// P0-4 batch-transaction rewrite (one commit+fsync per batch instead of per
// event) — see update/20260819-审计流水线容量-P0-4核证与批量投影方案.md.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx is a dialect-aware transaction: same ?→$n rewriting as DB, statements run
// inside one BEGIN…COMMIT (one WAL fsync at commit).
type Tx struct {
	tx *sql.Tx
	d  *DB
}

// BeginTx opens a batch transaction on the underlying pool.
func (d *DB) BeginTx(ctx context.Context) (*Tx, error) {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, d: d}, nil
}

func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.d.rewrite(query), args...)
}
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.d.rewrite(query), args...)
}
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.d.rewrite(query), args...)
}
func (t *Tx) Commit() error   { return t.tx.Commit() }
func (t *Tx) Rollback() error { return t.tx.Rollback() }

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

// ClampChars truncates s to at most maxChars Unicode characters (not bytes), so a
// value bound into a VARCHAR(n) column can never trip PostgreSQL's SQLSTATE 22001
// "value too long". Only for client-supplied DIAGNOSTIC free text (today:
// usage_event_ods.error_message); identifiers and metering fields are never
// clamped — an over-long identifier is a data error and must surface as one.
// maxChars <= 0 means "no clamp".
// bugfix: workflow/CI/bugfix/2026-09-23-collector-data-error-classified-transient.md
func ClampChars(s string, maxChars int) string {
	if maxChars <= 0 || utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	n := 0
	for i := range s {
		if n == maxChars {
			return s[:i]
		}
		n++
	}
	return s
}

// TerminalDataError marks a storage failure that is DETERMINISTIC for the same
// input — PostgreSQL SQLSTATE class 22 (data exception: value too long, bad
// encoding, numeric overflow) or 23 (integrity violation: NOT NULL, CHECK, FK).
// Re-sending the identical event can never succeed, so the ingest lanes answer
// it as a per-event terminal rejection inside a 200 instead of the whole-batch
// 503 that TransientStorageError produces. This is the collector-side instance
// of workflow/CI/designpattern/retryable-status-for-unservable.md: "I cannot
// serve" keeps a retryable status, "your data is wrong" must not — before this
// type every insert error was wrapped as transient and ONE 2049-character
// error_message made a proxy re-send the same batch every 30 s for two weeks
// (worker-1, 2026-09-09 → 09-23), which also starved its WAL prune.
// bugfix: workflow/CI/bugfix/2026-09-23-collector-data-error-classified-transient.md
type TerminalDataError struct {
	SQLState string
	Err      error
}

func (e *TerminalDataError) Error() string { return e.Err.Error() }
func (e *TerminalDataError) Unwrap() error { return e.Err }

// ClassifyStorageError reports whether err is a deterministic data error: a
// lib/pq error whose SQLSTATE class is 22 or 23. Everything else — connection
// loss (08), serialization (40), resource exhaustion (53), operator
// intervention (57), driver.ErrBadConn, context timeouts, SQLite errors,
// unknown drivers — stays transient. Unknown ⇒ transient is the conservative
// default P0-4 chose: a retry that might succeed must never become a silent
// loss. (Genuine duplicates never reach this function: inserts use
// ON CONFLICT DO NOTHING / INSERT OR IGNORE, so 23505 here would be a real
// constraint bug and is as deterministic as the rest of class 23.)
// lib/pq v1.12.3 exposes the code as a string type without a Class() method,
// hence the two-character prefix.
func ClassifyStorageError(err error) (sqlstate string, deterministic bool) {
	var pqErr *pq.Error
	if err == nil || !errors.As(err, &pqErr) {
		return "", false
	}
	code := string(pqErr.Code)
	if len(code) < 2 {
		return code, false
	}
	switch code[:2] {
	case "22", "23":
		return code, true
	}
	return code, false
}

// WrapStorageError is the one place an insert error becomes a wire
// classification: deterministic → *TerminalDataError (per-event terminal);
// otherwise the calling lane's own transient type via mkTransient (whole-batch
// 503, proxy re-sends from its WAL). Both ingest lanes (usage, conversation)
// call this so the split cannot drift between them.
func WrapStorageError(err error, mkTransient func(error) error) error {
	if code, deterministic := ClassifyStorageError(err); deterministic {
		return &TerminalDataError{SQLState: code, Err: err}
	}
	return mkTransient(err)
}
