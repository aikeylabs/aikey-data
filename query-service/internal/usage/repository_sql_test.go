package usage

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	_ "modernc.org/sqlite"
)

// The repo (sqlRepo) works against SQLite in-memory thanks to
// shared.DB abstracting the dialect. These tests exercise the SQLite
// code path (including the dialect-branching inside
// PersonalHourlyTimeline). A PG integration test would need a live
// Postgres instance, which we don't run in CI — the Postgres path is
// covered separately by the dialect unit tests in dbkit_test.go.

// setupUsageTestDB creates a minimal usage_fact_dwd table and seeds rows
// at specific hours so repository queries can be asserted deterministically.
func setupUsageTestDB(t *testing.T) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	// Schema is a trimmed subset of aikey-data/collector-service/migrations
	// — just enough columns for the personal* / master* queries. SQLite
	// is untyped so we don't bother with NOT NULL / defaults.
	//
	// event_time is INTEGER (Unix epoch millis) after v1.0.3-alpha to
	// match the real schema. Tests that insert timestamp values must
	// use int64 millis (see seedHourlyRows → time.Parse + UnixMilli).
	schema := `
	CREATE TABLE usage_fact_dwd (
		event_id         TEXT,
		org_id           TEXT,
		seat_id          TEXT,
		account_id       TEXT,
		virtual_key_id   TEXT,
		virtual_key_alias TEXT,
		provider_code    TEXT,
		protocol_type    TEXT,
		billing_scope    TEXT,
		event_time       INTEGER,  -- Unix epoch millis, UTC
		usage_date       TEXT,     -- YYYY-MM-DD
		total_tokens     INTEGER,
		request_count    INTEGER
	);`
	if _, err := raw.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

// seedHourlyRows inserts one row per (hour, tokens) entry at the given UTC date.
// event_time is written as int64 epoch millis — matches production schema post
// v1.0.3-alpha. Using time.Parse of an RFC3339 string makes the intent explicit
// ("2026-04-24T14:30:00Z" → millis) while staying self-contained in the test.
func seedHourlyRows(t *testing.T, db *shared.DB, date string, seatID string, rows []struct {
	hour   int
	tokens int64
	reqs   int64
}) {
	t.Helper()
	for i, r := range rows {
		iso := date + "T" + twoDigits(r.hour) + ":30:00Z"
		parsed, err := time.Parse(time.RFC3339, iso)
		if err != nil {
			t.Fatalf("parse %q: %v", iso, err)
		}
		eventTime := parsed.UTC().UnixMilli()
		_, err = db.DB.Exec(`
			INSERT INTO usage_fact_dwd
				(event_id, org_id, seat_id, event_time, usage_date, total_tokens, request_count)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			eventForHour(date, r.hour, i), "org1", seatID, eventTime, date, r.tokens, r.reqs)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}
func eventForHour(date string, hour, idx int) string {
	return date + "-h" + itoa(hour) + "-" + itoa(idx)
}

// --- PersonalHourlyTimeline ---

func TestPersonalHourlyTimeline_AggregatesByHour(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-04-24"
	seedHourlyRows(t, db, date, "seat1", []struct {
		hour   int
		tokens int64
		reqs   int64
	}{
		{hour: 9, tokens: 100, reqs: 1},
		{hour: 9, tokens: 50, reqs: 1},   // same hour — should aggregate
		{hour: 14, tokens: 200, reqs: 2},
		{hour: 23, tokens: 300, reqs: 3},
	})

	repo := NewSQLRepository(db)
	day, _ := time.Parse("2006-01-02", date)
	points, err := repo.PersonalHourlyTimeline(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: day,
	})
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("want 3 hour buckets (9, 14, 23), got %d: %+v", len(points), points)
	}

	byHour := map[int]HourlyPoint{}
	for _, p := range points {
		byHour[p.Hour] = p
	}
	if byHour[9].TotalTokens != 150 || byHour[9].RequestCount != 2 {
		t.Errorf("hour 9 aggregation wrong: %+v", byHour[9])
	}
	if byHour[14].TotalTokens != 200 || byHour[14].RequestCount != 2 {
		t.Errorf("hour 14 wrong: %+v", byHour[14])
	}
	if byHour[23].TotalTokens != 300 || byHour[23].RequestCount != 3 {
		t.Errorf("hour 23 wrong: %+v", byHour[23])
	}
}

func TestPersonalHourlyTimeline_ExcludesOtherDays(t *testing.T) {
	db := setupUsageTestDB(t)
	// Seed rows on 2026-04-23, 2026-04-24, 2026-04-25; query should only
	// pick up 04-24.
	for _, d := range []string{"2026-04-23", "2026-04-24", "2026-04-25"} {
		seedHourlyRows(t, db, d, "seat1", []struct {
			hour   int
			tokens int64
			reqs   int64
		}{
			{hour: 10, tokens: 777, reqs: 7},
		})
	}

	repo := NewSQLRepository(db)
	day, _ := time.Parse("2006-01-02", "2026-04-24")
	points, err := repo.PersonalHourlyTimeline(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: day,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Hour != 10 || points[0].TotalTokens != 777 {
		t.Errorf("expected single 04-24 row at hour 10 = 777, got %+v", points)
	}
}

func TestPersonalHourlyTimeline_FiltersBySeat(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-04-24"
	seedHourlyRows(t, db, date, "seat1", []struct {
		hour   int
		tokens int64
		reqs   int64
	}{{hour: 9, tokens: 100, reqs: 1}})
	seedHourlyRows(t, db, date, "seat2", []struct {
		hour   int
		tokens int64
		reqs   int64
	}{{hour: 9, tokens: 999, reqs: 9}})

	repo := NewSQLRepository(db)
	day, _ := time.Parse("2006-01-02", date)
	points, err := repo.PersonalHourlyTimeline(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: day,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].TotalTokens != 100 {
		t.Errorf("seat filter leak: %+v", points)
	}
}

func TestPersonalHourlyTimeline_EmptyDay(t *testing.T) {
	db := setupUsageTestDB(t)
	repo := NewSQLRepository(db)
	day, _ := time.Parse("2006-01-02", "2026-04-24")
	points, err := repo.PersonalHourlyTimeline(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: day,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 0 {
		t.Errorf("expected no rows for empty day, got %+v", points)
	}
}

// --- PersonalTimeline (daily) ---
// Baseline for Item 1 (DateString helper refactor). These tests
// exercise usage_date::text via stripPgCasts on SQLite, proving that
// the current behavior produces YYYY-MM-DD strings in the output.
//
// NOTE: Tests use a wide `startDate` (far earlier than any seeded
// row) to side-step a pre-existing modernc/sqlite quirk where
// time.Time is serialized as "YYYY-MM-DD HH:MM:SS+ZZ:ZZ". When that
// is used in `BETWEEN` against a shorter TEXT usage_date column the
// start-day rows are excluded by string compare. That's unrelated to
// the Item 1 refactor, which only touches how usage_date is
// projected in SELECT (::text cast vs helper), not the filter.

func insertDailyRow(t *testing.T, db *shared.DB, date, seatID, orgID, providerCode, billingScope string, tokens, reqs int64) {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, date+"T12:00:00Z")
	if err != nil {
		t.Fatalf("parse date %s: %v", date, err)
	}
	_, err = db.DB.Exec(`INSERT INTO usage_fact_dwd
		(event_id, org_id, seat_id, provider_code, billing_scope, event_time, usage_date, total_tokens, request_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		date+"-"+seatID+"-"+providerCode, orgID, seatID, providerCode, billingScope,
		parsed.UTC().UnixMilli(), date, tokens, reqs)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPersonalTimeline_AggregatesByDay(t *testing.T) {
	db := setupUsageTestDB(t)
	insertDailyRow(t, db, "2026-04-20", "seat1", "org1", "openai", "org_and_user", 100, 1)
	insertDailyRow(t, db, "2026-04-20", "seat1", "org1", "anthropic", "org_and_user", 50, 1)
	insertDailyRow(t, db, "2026-04-21", "seat1", "org1", "openai", "org_and_user", 200, 2)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-01") // wide margin — see file note
	end, _ := time.Parse("2006-01-02", "2026-04-30")
	points, err := repo.PersonalTimeline(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: start,
		EndDate:   end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("want 2 day rows, got %d: %+v", len(points), points)
	}
	// Scan result should be YYYY-MM-DD text (usage_date::text path).
	if points[0].Date != "2026-04-20" || points[0].TotalTokens != 150 || points[0].RequestCount != 2 {
		t.Errorf("day 0 wrong: %+v", points[0])
	}
	if points[1].Date != "2026-04-21" || points[1].TotalTokens != 200 || points[1].RequestCount != 2 {
		t.Errorf("day 1 wrong: %+v", points[1])
	}
}

func TestPersonalTimeline_FiltersBySeat(t *testing.T) {
	db := setupUsageTestDB(t)
	insertDailyRow(t, db, "2026-04-20", "seat1", "org1", "openai", "org_and_user", 100, 1)
	insertDailyRow(t, db, "2026-04-20", "seat2", "org1", "openai", "org_and_user", 999, 9)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-01")
	end, _ := time.Parse("2006-01-02", "2026-04-30")
	points, err := repo.PersonalTimeline(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: start,
		EndDate:   end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].TotalTokens != 100 {
		t.Errorf("seat filter leak: %+v", points)
	}
}

// --- PersonalByProtocolTimeline ---

func TestPersonalByProtocolTimeline_GroupsByDayAndProvider(t *testing.T) {
	db := setupUsageTestDB(t)
	insertDailyRow(t, db, "2026-04-20", "seat1", "org1", "openai", "org_and_user", 100, 1)
	insertDailyRow(t, db, "2026-04-20", "seat1", "org1", "anthropic", "org_and_user", 50, 1)
	insertDailyRow(t, db, "2026-04-21", "seat1", "org1", "openai", "org_and_user", 200, 2)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-01")
	end, _ := time.Parse("2006-01-02", "2026-04-30")
	points, err := repo.PersonalByProtocolTimeline(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: start,
		EndDate:   end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 {
		t.Fatalf("want 3 (day, protocol) rows, got %d: %+v", len(points), points)
	}
	// Verify dates are formatted as YYYY-MM-DD strings.
	for _, p := range points {
		if len(p.Date) != 10 || p.Date[4] != '-' || p.Date[7] != '-' {
			t.Errorf("expected YYYY-MM-DD date, got %q", p.Date)
		}
	}
}

// --- MasterTimeline ---

func TestMasterTimeline_FiltersByBillingScope(t *testing.T) {
	db := setupUsageTestDB(t)
	// Both rows for the same day, same org — only one should match the
	// billing_scope IN ('org_only','org_and_user') filter.
	insertDailyRow(t, db, "2026-04-20", "seat1", "org1", "openai", "org_and_user", 100, 1)
	insertDailyRow(t, db, "2026-04-20", "seat2", "org1", "openai", "user_only", 999, 9) // excluded

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-01")
	end, _ := time.Parse("2006-01-02", "2026-04-30")
	points, err := repo.MasterTimeline(context.Background(), QueryParams{
		OrgID:     "org1",
		StartDate: start,
		EndDate:   end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].TotalTokens != 100 {
		t.Errorf("billing_scope filter leak: %+v", points)
	}
	if points[0].Date != "2026-04-20" {
		t.Errorf("date format wrong: %q", points[0].Date)
	}
}

func TestMasterTimeline_FiltersByOrg(t *testing.T) {
	db := setupUsageTestDB(t)
	insertDailyRow(t, db, "2026-04-20", "seat1", "org1", "openai", "org_and_user", 100, 1)
	insertDailyRow(t, db, "2026-04-20", "seat1", "org2", "openai", "org_and_user", 999, 9)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-01")
	end, _ := time.Parse("2006-01-02", "2026-04-30")
	points, err := repo.MasterTimeline(context.Background(), QueryParams{
		OrgID:     "org1",
		StartDate: start,
		EndDate:   end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].TotalTokens != 100 {
		t.Errorf("org filter leak: %+v", points)
	}
}

func TestPersonalHourlyTimeline_OrgIDPersonal(t *testing.T) {
	// Personal-edition mode: events have org_id='personal', no seat_id.
	db := setupUsageTestDB(t)
	date := "2026-04-24"
	parsed, err := time.Parse(time.RFC3339, date+"T11:00:00Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = db.DB.Exec(`INSERT INTO usage_fact_dwd
		(event_id, org_id, event_time, usage_date, total_tokens, request_count)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"ev1", "personal", parsed.UTC().UnixMilli(), date, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewSQLRepository(db)
	day, _ := time.Parse("2006-01-02", date)
	points, err := repo.PersonalHourlyTimeline(context.Background(), QueryParams{
		OrgID:     "personal",
		StartDate: day,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Hour != 11 || points[0].TotalTokens != 42 {
		t.Errorf("personal-edition filter failed: %+v", points)
	}
}

// TestCrossDayBoundary_TodayUseAndTimelineAgree is the regression guard
// for bugfix 20260424. A user in +08:00 generates events whose local
// wall-clock falls on date D+1 while the UTC instant still belongs to
// day D. The Overview page must agree on three data surfaces:
//
//   - TODAY USE (hourly, filters event_time in [UTC-D, UTC-D+1))
//   - Token usage timeline (daily, filters usage_date BETWEEN ... )
//   - Provider breakdown (same usage_date filter)
//
// Pre-fix, event_time was stored as Go's default String format with a
// local tz suffix ("+0800 CST") and SQLite strftime returned NULL,
// so the hourly endpoint 500'd while the daily one showed the row.
// This test replicates the scenario by inserting a +08:00-local event
// whose UTC instant is today, then asserts all three surfaces include
// it with matching totals.
func TestCrossDayBoundary_TodayUseAndTimelineAgree(t *testing.T) {
	db := setupUsageTestDB(t)

	// Event at 2026-04-25T00:30:00+08:00 — UTC instant is
	// 2026-04-24T16:30:00Z, so usage_date = "2026-04-24" and hour 16.
	utcInstant, err := time.Parse(time.RFC3339, "2026-04-24T16:30:00Z")
	if err != nil {
		t.Fatalf("parse utc: %v", err)
	}
	ms := utcInstant.UTC().UnixMilli()
	const date = "2026-04-24"

	_, err = db.DB.Exec(`INSERT INTO usage_fact_dwd
		(event_id, org_id, seat_id, provider_code, billing_scope,
		 event_time, usage_date, total_tokens, request_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"boundary-ev1", "org1", "seat1", "anthropic", "org_and_user",
		ms, date, 1234, 1)
	if err != nil {
		t.Fatal(err)
	}

	repo := NewSQLRepository(db)

	// 1. TODAY USE card (hourly, queries event_time millis)
	day, _ := time.Parse("2006-01-02", date)
	hourly, err := repo.PersonalHourlyTimeline(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: day,
	})
	if err != nil {
		t.Fatalf("hourly: %v", err)
	}
	if len(hourly) != 1 || hourly[0].Hour != 16 || hourly[0].TotalTokens != 1234 {
		t.Errorf("TODAY USE must include the boundary event at hour 16: %+v", hourly)
	}

	// 2. Token usage timeline (daily, queries usage_date)
	start, _ := time.Parse("2006-01-02", "2026-04-01")
	end, _ := time.Parse("2006-01-02", "2026-04-30")
	timeline, err := repo.PersonalTimeline(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: start,
		EndDate:   end,
	})
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	found := false
	for _, p := range timeline {
		if p.Date == date {
			found = true
			if p.TotalTokens != 1234 {
				t.Errorf("timeline %s tokens = %d, want 1234", date, p.TotalTokens)
			}
		}
	}
	if !found {
		t.Errorf("Token usage must include %s row: %+v", date, timeline)
	}

	// 3. Provider breakdown (daily total, queries usage_date)
	byProto, err := repo.PersonalByProtocolTotal(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: start,
		EndDate:   end,
	})
	if err != nil {
		t.Fatalf("by-proto: %v", err)
	}
	if len(byProto) != 1 || byProto[0].ProtocolType != "anthropic" || byProto[0].TotalTokens != 1234 {
		t.Errorf("Provider breakdown must show 1234 tokens for anthropic: %+v", byProto)
	}
}

// TestPersonalHourlyTimeline_LocalTZShiftsHourBucket is the regression
// guard for the tz-local round. The same physical event at UTC 04:00
// must appear at hour 4 in a UTC query and hour 12 in an
// Asia/Shanghai query (+08:00).
func TestPersonalHourlyTimeline_LocalTZShiftsHourBucket(t *testing.T) {
	db := setupUsageTestDB(t)

	a, _ := time.Parse(time.RFC3339, "2026-04-24T04:00:00Z")
	_, err := db.DB.Exec(`INSERT INTO usage_fact_dwd
		(event_id, org_id, seat_id, event_time, usage_date, total_tokens, request_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"evA", "org1", "seat1", a.UTC().UnixMilli(), "2026-04-24", 100, 1)
	if err != nil {
		t.Fatal(err)
	}

	repo := NewSQLRepository(db)
	day, _ := time.Parse("2006-01-02", "2026-04-24")

	t.Run("UTC shows hour 4", func(t *testing.T) {
		p := QueryParams{SeatID: "seat1", StartDate: day, TZ: "UTC"}
		p.Defaults()
		p.EndDate = p.StartDate
		points, err := repo.PersonalHourlyTimeline(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if len(points) != 1 || points[0].Hour != 4 || points[0].TotalTokens != 100 {
			t.Errorf("UTC path: want hour=4/tokens=100, got %+v", points)
		}
	})

	t.Run("Asia/Shanghai shows hour 12", func(t *testing.T) {
		p := QueryParams{SeatID: "seat1", StartDate: day, TZ: "Asia/Shanghai"}
		p.Defaults()
		p.EndDate = p.StartDate
		points, err := repo.PersonalHourlyTimeline(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if len(points) != 1 || points[0].Hour != 12 || points[0].TotalTokens != 100 {
			t.Errorf("Asia/Shanghai path: want hour=12/tokens=100, got %+v", points)
		}
	})
}

// TestPersonalTimeline_LocalTZDayBoundary verifies that an event whose
// UTC date and local date differ lands in the *local* day bucket.
// 2026-04-25 01:00 +0800 = 2026-04-24 17:00 UTC: a Shanghai user
// expects it on 04-25; a UTC user expects it on 04-24. Both correct.
func TestPersonalTimeline_LocalTZDayBoundary(t *testing.T) {
	db := setupUsageTestDB(t)

	b, _ := time.Parse(time.RFC3339, "2026-04-24T17:00:00Z")
	_, err := db.DB.Exec(`INSERT INTO usage_fact_dwd
		(event_id, org_id, seat_id, event_time, usage_date, total_tokens, request_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"evB", "org1", "seat1", b.UTC().UnixMilli(), "2026-04-24", 200, 1)
	if err != nil {
		t.Fatal(err)
	}

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-23")
	end, _ := time.Parse("2006-01-02", "2026-04-26")

	t.Run("UTC buckets on 2026-04-24", func(t *testing.T) {
		p := QueryParams{SeatID: "seat1", StartDate: start, EndDate: end, TZ: "UTC"}
		p.Defaults()
		tl, err := repo.PersonalTimeline(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if len(tl) != 1 || tl[0].Date != "2026-04-24" || tl[0].TotalTokens != 200 {
			t.Errorf("UTC bucketing wrong: %+v", tl)
		}
	})

	t.Run("Asia/Shanghai buckets on 2026-04-25", func(t *testing.T) {
		p := QueryParams{SeatID: "seat1", StartDate: start, EndDate: end, TZ: "Asia/Shanghai"}
		p.Defaults()
		tl, err := repo.PersonalTimeline(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if len(tl) != 1 || tl[0].Date != "2026-04-25" || tl[0].TotalTokens != 200 {
			t.Errorf("Shanghai bucketing wrong — event should land in local 04-25: %+v", tl)
		}
	})
}
