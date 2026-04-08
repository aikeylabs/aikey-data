package shared

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
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
