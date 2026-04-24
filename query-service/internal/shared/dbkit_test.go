package shared

import (
	"strings"
	"testing"
)

// --- rewrite() ---

func TestRewrite_PostgresPlaceholders(t *testing.T) {
	d := &DB{Dialect: DialectPostgres}
	got := d.rewrite("SELECT * FROM t WHERE a = ? AND b = ? AND c = ?")
	want := "SELECT * FROM t WHERE a = $1 AND b = $2 AND c = $3"
	if got != want {
		t.Fatalf("rewrite PG placeholders: got %q, want %q", got, want)
	}
}

func TestRewrite_SQLiteKeepsPlaceholders(t *testing.T) {
	d := &DB{Dialect: DialectSQLite}
	got := d.rewrite("SELECT * FROM t WHERE a = ? AND b = ?")
	want := "SELECT * FROM t WHERE a = ? AND b = ?"
	if got != want {
		t.Fatalf("rewrite SQLite should pass through: got %q, want %q", got, want)
	}
}

func TestRewrite_SQLiteStripsCasts(t *testing.T) {
	d := &DB{Dialect: DialectSQLite}
	got := d.rewrite("SELECT x::text, y::integer, z::bigint FROM t WHERE a = ?")
	want := "SELECT x, y, z FROM t WHERE a = ?"
	if got != want {
		t.Fatalf("SQLite should strip PG casts: got %q, want %q", got, want)
	}
}

func TestRewrite_PlaceholderInsideStringLiteralIgnored(t *testing.T) {
	d := &DB{Dialect: DialectPostgres}
	// A literal ? inside a single-quoted string must NOT be rewritten.
	got := d.rewrite("SELECT 'what?' FROM t WHERE a = ?")
	want := "SELECT 'what?' FROM t WHERE a = $1"
	if got != want {
		t.Fatalf("quoted ? must stay literal: got %q, want %q", got, want)
	}
}

// --- Now() ---

func TestNow_Dialect(t *testing.T) {
	if got := (&DB{Dialect: DialectPostgres}).Now(); got != "NOW()" {
		t.Errorf("PG Now() = %q, want NOW()", got)
	}
	if got := (&DB{Dialect: DialectSQLite}).Now(); got != "datetime('now')" {
		t.Errorf("SQLite Now() = %q, want datetime('now')", got)
	}
}

// --- IsSQLite() ---

func TestIsSQLite(t *testing.T) {
	if (&DB{Dialect: DialectPostgres}).IsSQLite() {
		t.Error("Postgres should not report IsSQLite")
	}
	if !(&DB{Dialect: DialectSQLite}).IsSQLite() {
		t.Error("SQLite should report IsSQLite")
	}
}

// --- InsertOrIgnore / InsertOrIgnoreOn ---

func TestInsertOrIgnore_Dialect(t *testing.T) {
	pg := (&DB{Dialect: DialectPostgres}).InsertOrIgnore("t", "a, b", "?, ?")
	if !strings.Contains(pg, "ON CONFLICT DO NOTHING") {
		t.Errorf("PG InsertOrIgnore missing ON CONFLICT: %q", pg)
	}
	sq := (&DB{Dialect: DialectSQLite}).InsertOrIgnore("t", "a, b", "?, ?")
	if !strings.HasPrefix(sq, "INSERT OR IGNORE") {
		t.Errorf("SQLite InsertOrIgnore missing INSERT OR IGNORE: %q", sq)
	}
}

func TestInsertOrIgnoreOn_Dialect(t *testing.T) {
	pg := (&DB{Dialect: DialectPostgres}).InsertOrIgnoreOn("t", "a, b", "?, ?", "a")
	if !strings.Contains(pg, "ON CONFLICT (a) DO NOTHING") {
		t.Errorf("PG InsertOrIgnoreOn missing ON CONFLICT (a): %q", pg)
	}
	sq := (&DB{Dialect: DialectSQLite}).InsertOrIgnoreOn("t", "a, b", "?, ?", "a")
	if !strings.HasPrefix(sq, "INSERT OR IGNORE") {
		t.Errorf("SQLite InsertOrIgnoreOn should still use INSERT OR IGNORE (no conflict-cols clause needed): %q", sq)
	}
}

// --- HourBucket ---

func TestHourBucket_Postgres(t *testing.T) {
	d := &DB{Dialect: DialectPostgres}
	got := d.HourBucket("event_time")
	want := "CAST(EXTRACT(HOUR FROM event_time AT TIME ZONE 'UTC') AS INTEGER)"
	if got != want {
		t.Fatalf("PG HourBucket:\n got %q\nwant %q", got, want)
	}
}

func TestHourBucket_SQLite(t *testing.T) {
	d := &DB{Dialect: DialectSQLite}
	got := d.HourBucket("event_time")
	// Post-v1.0.3-alpha: event_time is INT64 millis, extract via unixepoch.
	want := "CAST(strftime('%H', event_time / 1000, 'unixepoch') AS INTEGER)"
	if got != want {
		t.Fatalf("SQLite HourBucket:\n got %q\nwant %q", got, want)
	}
}

func TestHourBucket_CustomColumn(t *testing.T) {
	d := &DB{Dialect: DialectSQLite}
	got := d.HourBucket("occurred_at")
	if !strings.Contains(got, "occurred_at") {
		t.Fatalf("HourBucket should interpolate column name: %q", got)
	}
}

// --- DateString ---

func TestDateString_Postgres(t *testing.T) {
	d := &DB{Dialect: DialectPostgres}
	if got := d.DateString("usage_date"); got != "usage_date::text" {
		t.Fatalf("PG DateString: got %q, want usage_date::text", got)
	}
}

func TestDateString_SQLite(t *testing.T) {
	d := &DB{Dialect: DialectSQLite}
	if got := d.DateString("usage_date"); got != "usage_date" {
		t.Fatalf("SQLite DateString: got %q, want usage_date (no cast)", got)
	}
}

// --- stripPgCasts (directly) ---

func TestStripPgCasts(t *testing.T) {
	cases := map[string]string{
		"SELECT a::text":              "SELECT a",
		"SELECT a::text, b::integer":  "SELECT a, b",
		"no casts here":               "no casts here",
		"a::BigInt mixed case":        "a mixed case",
	}
	for in, want := range cases {
		if got := stripPgCasts(in); got != want {
			t.Errorf("stripPgCasts(%q) = %q, want %q", in, got, want)
		}
	}
}
