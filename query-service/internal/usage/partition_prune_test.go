package usage

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

// pgDB is a dialect-only stub: partitionPrune and personalFilter build strings
// and never touch the connection, so no database is needed to pin their shape.
func pgDB() *shared.DB { return &shared.DB{Dialect: shared.DialectPostgres} }

// partitionPruneClauseRe pins the ONLY shape partitionPrune may emit.
//
// 🔴 It is what makes the literals safe. partitionPrune inlines its two dates
// instead of binding them (so that Postgres prunes at PLAN time, and so that
// none of the eleven callers has to touch its positional arg list). Inlining is
// only defensible while the values provably cannot be anything but ten
// characters of digits and dashes — this regex is that proof, checked on every
// window the tests below throw at it.
var partitionPruneClauseRe = regexp.MustCompile(
	`^usage_date >= '\d{4}-\d{2}-\d{2}' AND usage_date <= '\d{4}-\d{2}-\d{2}' AND $`)

func mustDay(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("bad date %q: %v", s, err)
	}
	return d
}

// The prune must never emit anything but the pinned literal shape.
func TestPartitionPrune_EmitsOnlyTheSafeLiteralShape(t *testing.T) {
	for _, tc := range []struct{ name, start, end string }{
		{"single day", "2026-04-24", "2026-04-24"},
		{"month span", "2026-01-01", "2026-01-31"},
		{"year boundary", "2025-12-31", "2026-01-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := partitionPrune(pgDB(), QueryParams{StartDate: mustDay(t, tc.start), EndDate: mustDay(t, tc.end)})
			if !partitionPruneClauseRe.MatchString(got) {
				t.Fatalf("prune clause escaped its pinned shape: %q\n"+
					"the literals are only safe while this shape holds — bind them if it must change", got)
			}
		})
	}
}

// A window we cannot trust must produce NO prune, never a narrow one.
//
// 🔴 This is the case that went red during development and is the whole reason
// the guard exists: LocalWindowMs turns a zero EndDate into year 1, which would
// have emitted `usage_date >= '2026-04-23' AND usage_date <= '0001-01-03'` —
// an empty range that silently drops every row. Slow is acceptable; quietly
// wrong is not.
func TestPartitionPrune_SkipsRatherThanNarrowsOnUntrustedWindows(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    QueryParams
	}{
		{"no dates at all", QueryParams{}},
		{"start only (EndDate zero -> year 1)", QueryParams{StartDate: mustDay(t, "2026-04-24")}},
		{"end before start", QueryParams{StartDate: mustDay(t, "2026-04-24"), EndDate: mustDay(t, "2026-04-01")}},
		{"pre-2000 window", QueryParams{StartDate: mustDay(t, "1999-01-01"), EndDate: mustDay(t, "1999-01-02")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := partitionPrune(pgDB(), tc.p); got != "" {
				t.Fatalf("emitted a prune for an untrusted window: %q — this is how rows disappear", got)
			}
		})
	}
}

// The prune must be a strict SUPERSET of the exact event_time window: adding it
// may not change a single answer.
//
// 🔴 Property, not example. It seeds rows on both sides of every boundary the
// prune could get wrong — the day before, the first and last hour of the window,
// the day after — and asserts the pruned repository returns exactly what the
// exact filter alone would. A prune that is off by one day fails here.
func TestPartitionPrune_DoesNotChangeAnyResult(t *testing.T) {
	db := setupUsageTestDB(t)
	repo := NewSQLRepository(db)

	// One row per day across a five-day span, all for the same seat.
	days := []string{"2026-04-22", "2026-04-23", "2026-04-24", "2026-04-25", "2026-04-26"}
	for i, d := range days {
		seedHourlyRows(t, db, d, "seat1", []struct {
			hour   int
			tokens int64
			reqs   int64
		}{{hour: 12, tokens: int64(10 * (i + 1)), reqs: 1}})
	}

	// Query the middle three days. The prune widens to 04-22..04-27, so the
	// rows outside the window are reachable by the prune and MUST still be
	// excluded — by the exact event_time filter that stays in the query.
	p := QueryParams{
		SeatID:    "seat1",
		StartDate: mustDay(t, "2026-04-23"),
		EndDate:   mustDay(t, "2026-04-25"),
	}
	// The repository fixture is SQLite, where the prune is deliberately OFF.
	// This case therefore pins the OTHER half of the contract: turning the
	// prune off for SQLite must not have disturbed the answers either.
	if got := partitionPrune(db, p); got != "" {
		t.Fatalf("SQLite emitted a prune (%q) — it has no partitions to prune and a legacy row with an empty usage_date would simply vanish", got)
	}

	points, err := repo.PersonalTimeline(context.Background(), p)
	if err != nil {
		t.Fatalf("PersonalTimeline: %v", err)
	}
	var total int64
	for _, pt := range points {
		total += pt.TotalTokens
	}
	// days 04-23, 04-24, 04-25 -> 20 + 30 + 40
	const want = 90
	if total != want {
		t.Fatalf("pruned query returned %d tokens, want %d (%d points)\n"+
			"either the prune dropped an in-window day, or it let an out-of-window day through",
			total, want, len(points))
	}
}

// Every personal-scoped query must carry the prune.
//
// 🔴 Fenced at the CHOKE POINT, not per query. personalFilter is the one place
// all eleven personal endpoints get their predicate, which is exactly why the
// prune was put there — a twelfth endpoint gets it for free. If someone moves
// the prune out of personalFilter to "keep it simple", this goes red.
func TestPartitionPrune_IsAttachedAtTheSharedChokePoint(t *testing.T) {
	p := QueryParams{
		SeatID:    "seat1",
		StartDate: mustDay(t, "2026-04-23"),
		EndDate:   mustDay(t, "2026-04-25"),
	}
	clause, _ := (&sqlRepo{db: pgDB()}).personalFilter(p)
	if !regexp.MustCompile(`^usage_date >= '\d{4}-\d{2}-\d{2}' AND usage_date <= '\d{4}-\d{2}-\d{2}' AND `).MatchString(clause) {
		t.Fatalf("personalFilter no longer prefixes the partition prune: %q\n"+
			"without it every personal query sequentially scans every partition, forever", clause)
	}
	// And the arg shape the eleven callers depend on must be unchanged: the
	// prune contributes literals, never placeholders.
	if got := countPlaceholders(clause); got != 1 {
		t.Fatalf("personalFilter clause now has %d placeholders, want 1 — "+
			"every caller passes args positionally, so this silently misbinds them", got)
	}
}

func countPlaceholders(s string) int {
	n := 0
	for _, r := range s {
		if r == '?' {
			n++
		}
	}
	return n
}


// SQLite must never see the prune.
//
// 🔴 The gate is the risk control, not an optimisation detail. SQLite does not
// partition this table, so the prune cannot speed anything up there — while a
// Personal database written before usage_date was populated would hold '' in
// that column, and '' sorts below every real date, so every one of that user's
// rows would silently disappear. Postgres cannot hit that: usage_date is the
// partition key and therefore NOT NULL. Benefit and risk fall on opposite
// sides of this line; the gate keeps only the benefit.
func TestPartitionPrune_IsPostgresOnly(t *testing.T) {
	p := QueryParams{
		SeatID:    "seat1",
		StartDate: mustDay(t, "2026-04-23"),
		EndDate:   mustDay(t, "2026-04-25"),
	}
	if got := partitionPrune(&shared.DB{Dialect: shared.DialectSQLite}, p); got != "" {
		t.Fatalf("SQLite prune = %q, want none", got)
	}
	if got := partitionPrune(nil, p); got != "" {
		t.Fatalf("nil-dialect prune = %q, want none — unknown dialect must not gamble", got)
	}
	if got := partitionPrune(pgDB(), p); got == "" {
		t.Fatal("Postgres emitted no prune — the whole point of the change is gone")
	}
}

// The prune must be WIDER than the exact window on both ends.
//
// 🔴 This is the superset property, checked on the string rather than on rows,
// because the rows live in Postgres and the repository fixture is SQLite. An
// off-by-one that narrowed either bound would drop a real day of usage.
func TestPartitionPrune_IsWiderThanTheExactWindow(t *testing.T) {
	start, end := mustDay(t, "2026-04-23"), mustDay(t, "2026-04-25")
	got := partitionPrune(pgDB(), QueryParams{StartDate: start, EndDate: end})
	m := regexp.MustCompile(`usage_date >= '(\d{4}-\d{2}-\d{2})' AND usage_date <= '(\d{4}-\d{2}-\d{2})'`).FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("cannot read bounds out of %q", got)
	}
	lo, hi := mustDay(t, m[1]), mustDay(t, m[2])
	// LocalWindowMs ends at EndDate+1day; the prune must sit outside both ends.
	if !lo.Before(start) {
		t.Fatalf("low bound %s is not before the window start %s — a day of usage can be cut off",
			m[1], start.Format("2006-01-02"))
	}
	if !hi.After(end.AddDate(0, 0, 1)) {
		t.Fatalf("high bound %s is not after the window end %s — a day of usage can be cut off",
			m[2], end.AddDate(0, 0, 1).Format("2006-01-02"))
	}
}
