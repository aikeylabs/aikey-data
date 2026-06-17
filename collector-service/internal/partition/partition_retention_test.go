package partition

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

func TestPartitionsToDropBefore(t *testing.T) {
	names := []string{
		"conversation_records_2025_01", // drop (< 2026-01)
		"conversation_records_2025_12", // drop
		"conversation_records_2026_01", // keep (== cutoff month)
		"conversation_records_2026_06", // keep (newer)
		"conversation_records_default", // DEFAULT partition — never drop
		"conversation_records_bogus",   // non-monthly child — skip
		"other_table_2025_01",          // wrong parent prefix — skip
	}
	cutoff := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC) // cutoff month = 2026-01
	got := partitionsToDropBefore(names, "conversation_records", cutoff)
	want := []string{"conversation_records_2025_01", "conversation_records_2025_12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("partitionsToDropBefore = %v, want %v (drop only months strictly before the cutoff month; keep cutoff month + newer; skip DEFAULT/non-monthly/foreign)", got, want)
	}
}

func TestParseMonthSuffix(t *testing.T) {
	const prefix = "conversation_records_"
	cases := []struct {
		name string
		y, m int
		ok   bool
	}{
		{"conversation_records_2026_06", 2026, 6, true},
		{"conversation_records_2025_12", 2025, 12, true},
		{"conversation_records_default", 0, 0, false}, // DEFAULT partition
		{"conversation_records_2026_13", 0, 0, false}, // month out of range
		{"conversation_records_2026_00", 0, 0, false}, // month 0
		{"conversation_records_26_6", 0, 0, false},    // wrong widths
		{"conversation_records_abcd_06", 0, 0, false}, // non-numeric year
		{"other_table_2026_06", 0, 0, false},          // wrong prefix
	}
	for _, c := range cases {
		y, m, ok := parseMonthSuffix(c.name, prefix)
		if ok != c.ok || y != c.y || m != c.m {
			t.Fatalf("parseMonthSuffix(%q) = (%d,%d,%v), want (%d,%d,%v)", c.name, y, m, ok, c.y, c.m, c.ok)
		}
	}
}

// SQLite is not partitioned — DropPartitionsBefore must no-op (Cluster
// conversation-audit runs on PostgreSQL). nil *sql.DB is safe: the dialect guard
// returns before any query.
func TestDropPartitionsBefore_SQLiteNoop(t *testing.T) {
	db := shared.NewDB(nil, shared.DialectSQLite)
	dropped, err := DropPartitionsBefore(context.Background(), db, "conversation_records", time.Now())
	if err != nil || dropped != nil {
		t.Fatalf("SQLite DropPartitionsBefore = (%v, %v), want (nil, nil) no-op", dropped, err)
	}
}
