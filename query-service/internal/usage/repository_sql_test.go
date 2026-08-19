package usage

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/baseline"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	_ "modernc.org/sqlite"
)

// The repo (sqlRepo) works against SQLite in-memory thanks to
// shared.DB abstracting the dialect. These tests exercise the SQLite
// code path (including the dialect-branching inside
// PersonalHourlyTimeline). A PG integration test would need a live
// Postgres instance, which we don't run in CI — the Postgres path is
// covered separately by the dialect unit tests in dbkit_test.go.

// setupUsageTestDB bootstraps an in-memory SQLite with the **real
// v1.0.0 baseline data schema** (usage_event_ods + usage_fact_dwd +
// usage_dwd_projector_tasks) so repository tests run against the
// production-shape tables — including the NOT NULL / UNIQUE / DEFAULT
// constraints. See
// workflow/CI/IDE/claude/principles/test-fixture-real-schema.md for
// why inline simplified CREATE TABLEs are no longer acceptable.
//
// The DDL is sourced from the aikey-data/baseline public package
// (which is the same source aikey-config-tool's migration registry
// delegates to for ComponentData), so a drift between fixture and
// production schema becomes a compile / build error rather than a
// silent test miscoverage.
func setupUsageTestDB(t *testing.T) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, stmt := range baseline.DDLFor(baseline.ComponentData, baseline.DialectSQLite) {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("baseline DDL exec failed: %v\nstmt prefix=%.120s", err, stmt)
		}
	}
	// Post-baseline migrations the production boot path would run.
	// We mirror them by hand here rather than depend on
	// `aikey-config-tool/pkg/dbmigrate` from a query-service test —
	// the registry is owned by config-tool and pulling it in adds a
	// new cross-repo dep just so tests can replay migrations. Keep
	// these in lock-step with the corresponding migration entry
	// (file path printed below) so a future column add is caught at
	// review time by the matching diff in both places.
	postBaseline := []string{
		// aikey-config-tool/pkg/dbmigrate/versions/v1_0_0_rc5_app_slug.go
		`ALTER TABLE usage_event_ods ADD COLUMN app_slug TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN app_slug TEXT`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_app_slug_date ON usage_fact_dwd (app_slug, usage_date) WHERE app_slug IS NOT NULL AND app_slug != ''`,
		// aikey-config-tool/pkg/dbmigrate/versions/v1_0_0_rc6_session_id.go
		`ALTER TABLE usage_event_ods ADD COLUMN session_id TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN session_id TEXT`,
		// aikey-config-tool/pkg/dbmigrate/versions/v1_0_0_rc8_pricing.go
		// (cost-pricing audit columns + pending-pricing / snapshot tables).
		`ALTER TABLE usage_event_ods ADD COLUMN region TEXT`,
		`ALTER TABLE usage_event_ods ADD COLUMN endpoint_url TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN billing_period TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN unit_prices_snapshot TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN pricing_snapshot_id TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN region TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN endpoint_url TEXT`,
		`CREATE TABLE IF NOT EXISTS unpriced_models (
			model         TEXT NOT NULL,
			provider      TEXT NOT NULL,
			first_seen_at INTEGER NOT NULL,
			last_seen_at  INTEGER NOT NULL,
			event_count   INTEGER NOT NULL DEFAULT 0,
			status        TEXT NOT NULL DEFAULT 'pending',
			notes         TEXT,
			PRIMARY KEY (model, provider)
		)`,
		`CREATE TABLE IF NOT EXISTS pricing_snapshots (
			snapshot_id      TEXT NOT NULL,
			litellm_sha256   TEXT NOT NULL,
			history_sha256   TEXT NOT NULL,
			overrides_sha256 TEXT NOT NULL,
			aikey_version    TEXT,
			created_at       INTEGER NOT NULL,
			effective_from   INTEGER NOT NULL,
			effective_until  INTEGER,
			notes            TEXT,
			PRIMARY KEY (snapshot_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pricing_snapshots_active ON pricing_snapshots (effective_until) WHERE effective_until IS NULL`,
		// aikey-config-tool/pkg/dbmigrate/versions/v1_0_1_alpha1_oauth_identity.go
		`ALTER TABLE usage_fact_dwd  ADD COLUMN oauth_identity TEXT`,
		// aikey-config-tool/pkg/dbmigrate/versions/v1_0_1_alpha1_dwd_integrity_columns.go
		`ALTER TABLE usage_fact_dwd  ADD COLUMN content_hash TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN source_id TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN source_seq INTEGER`,
		// aikey-config-tool/pkg/dbmigrate/versions/v1_0_1_alpha5_dwd_request_id.go
		`ALTER TABLE usage_fact_dwd  ADD COLUMN request_id TEXT`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_org_request_id ON usage_fact_dwd (org_id, request_id) WHERE request_id IS NOT NULL AND request_id != ''`,
		// aikey-config-tool/pkg/dbmigrate/versions/v1_0_1_alpha5_dwd_fallback_attribution.go
		`ALTER TABLE usage_fact_dwd  ADD COLUMN fallback_reason TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN fallback_attempt INTEGER`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_fallback_recent ON usage_fact_dwd (org_id, occurred_at, provider_code) WHERE fallback_attempt IS NOT NULL AND fallback_attempt > 1`,
		usageReportingViewFixtureSQL,
	}
	for _, stmt := range postBaseline {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("post-baseline migration exec failed: %v\nstmt=%s", err, stmt)
		}
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

const usageReportingViewFixtureSQL = `
CREATE VIEW usage_reporting_fact AS
SELECT d.*,
       CASE
         WHEN NULLIF(d.request_id, '') IS NULL THEN d.request_count
         WHEN NOT EXISTS (
           SELECT 1 FROM usage_fact_dwd peer
            WHERE COALESCE(peer.org_id, '') = COALESCE(d.org_id, '')
              AND COALESCE(peer.account_id, '') = COALESCE(d.account_id, '')
              AND COALESCE(peer.seat_id, '') = COALESCE(d.seat_id, '')
              AND peer.request_id = d.request_id
              AND (
                CASE WHEN peer.request_status = 'success' THEN 1 ELSE 0 END
                  > CASE WHEN d.request_status = 'success' THEN 1 ELSE 0 END
                OR (
                  CASE WHEN peer.request_status = 'success' THEN 1 ELSE 0 END
                    = CASE WHEN d.request_status = 'success' THEN 1 ELSE 0 END
                  AND (peer.event_time > d.event_time
                    OR (peer.event_time = d.event_time AND peer.dwd_id > d.dwd_id))
                )
              )
         ) THEN 1 ELSE 0
       END AS client_request_count
  FROM usage_fact_dwd d`

// dwdRow describes a single usage_fact_dwd row to seed via insertDWD.
// Only fields the queries under test actually read are exposed; every
// NOT NULL column not listed here gets a sensible default
// (request_status='success', completion_source='test',
// quality_status='ok', user_usage_scope='normal',
// projector_version='test', billing_scope='user_only') inside
// insertDWD so the baseline schema's constraints are satisfied without
// per-test boilerplate.
//
// Model: pass the literal sentinel "<NULL>" to test the SQL NULL
// branch (insertDWD translates it to `nil`); pass `""` to test the
// empty-string branch; any other value is stored verbatim. This
// asymmetry exists so the empty-string case stays addressable from
// table-driven tests without resorting to *string everywhere.
type dwdRow struct {
	EventID                  string // required (NOT NULL UNIQUE per org_id)
	RequestID                string
	OrgID                    string
	SeatID                   string
	AccountID                string
	VirtualKeyID             string
	VirtualKeyAlias          string
	ProviderCode             string
	ProtocolType             string
	Model                    string // "<NULL>" sentinel → SQL NULL; "" stored as empty string
	EventTimeMs              int64
	UsageDate                string // YYYY-MM-DD
	TotalTokens              int64
	RequestCount             int64 // defaults to 1 if zero
	InputTokens              int64
	CachedInputTokens        int64
	CacheCreationInputTokens int64
	OutputTokens             int64
	BillingScope             string // defaults to "user_only" if empty
	AppSlug                  string // empty stored as empty string (matches production ingest)
	// BillableAmount nil → billable_amount + currency stored as NULL (an
	// "unpriced" row, what the projector writes on a price-table miss);
	// non-nil → stored with Currency (default "USD"). Lets cost-aggregation
	// tests seed mixed priced / unpriced rows without *float64 everywhere.
	BillableAmount *float64
	Currency       string // defaults to "USD" when BillableAmount != nil
	// UserUsageScope defaults to "normal". Set "non_generation" / "excluded" /
	// "abnormal" to exercise the 2026-07-15 scope filters.
	UserUsageScope string
	RequestStatus  string // defaults to "success"
	HTTPStatusCode int
	// Audit filter dimensions (20260729 用量审计页自由筛选 fence tests).
	// Defaults preserve the pre-filter fixture behaviour: quality_status "ok",
	// credential_id / anomaly_type NULL.
	CredentialID  string // empty → NULL
	QualityStatus string // empty → "ok" (the long-standing fixture default)
	AnomalyType   string // empty → NULL
	OAuthIdentity string // empty → NULL
}

// odsIDSeq generates unique ods_id values per inserted DWD row so the
// `UNIQUE (ods_id)` constraint doesn't collide within or across tests.
// Starts high enough that production-style ODS sequences seeded in
// future integration tests don't overlap.
var odsIDSeq int64 = 1_000_000

// insertDWD seeds one row into usage_fact_dwd, filling all baseline
// NOT NULL columns. Returns nothing — caller doesn't need the assigned
// dwd_id (SQLite AUTOINCREMENT) for any current assertion.
func insertDWD(t *testing.T, db *shared.DB, r dwdRow) {
	t.Helper()
	if r.EventID == "" {
		t.Fatalf("insertDWD: event_id is required")
	}
	if r.RequestCount == 0 {
		r.RequestCount = 1
	}
	if r.BillingScope == "" {
		r.BillingScope = "user_only"
	}
	if r.UserUsageScope == "" {
		r.UserUsageScope = "normal"
	}
	if r.RequestStatus == "" {
		r.RequestStatus = "success"
	}
	odsIDSeq++
	var modelArg any = r.Model
	if r.Model == "<NULL>" {
		modelArg = nil
	}
	if r.QualityStatus == "" {
		r.QualityStatus = "ok"
	}
	var credentialArg, anomalyArg, identityArg any
	if r.CredentialID != "" {
		credentialArg = r.CredentialID
	}
	if r.AnomalyType != "" {
		anomalyArg = r.AnomalyType
	}
	if r.OAuthIdentity != "" {
		identityArg = r.OAuthIdentity
	}
	// Priced row → store billable_amount + currency; unpriced row (nil)
	// → NULL both, matching what the projector writes on a price miss.
	var billableArg, currencyArg any
	if r.BillableAmount != nil {
		billableArg = *r.BillableAmount
		cur := r.Currency
		if cur == "" {
			cur = "USD"
		}
		currencyArg = cur
	}
	_, err := db.DB.Exec(`
		INSERT INTO usage_fact_dwd (
			event_id, request_id, ods_id, occurred_at, event_time, usage_date,
			org_id, account_id, seat_id, virtual_key_id, virtual_key_alias,
			provider_code, protocol_type, model,
			request_count, total_tokens,
			input_tokens, cached_input_tokens, cache_creation_input_tokens, output_tokens,
			request_status, http_status_code, completion_source, quality_status,
			billing_scope, user_usage_scope, projector_version,
			app_slug, billable_amount, currency,
			credential_id, anomaly_type, oauth_identity
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.EventID, r.RequestID, odsIDSeq, r.EventTimeMs, r.EventTimeMs, r.UsageDate,
		r.OrgID, r.AccountID, r.SeatID, r.VirtualKeyID, r.VirtualKeyAlias,
		r.ProviderCode, r.ProtocolType, modelArg,
		r.RequestCount, r.TotalTokens,
		r.InputTokens, r.CachedInputTokens, r.CacheCreationInputTokens, r.OutputTokens,
		r.RequestStatus, r.HTTPStatusCode, "test", r.QualityStatus,
		r.BillingScope, r.UserUsageScope, "test",
		r.AppSlug, billableArg, currencyArg,
		credentialArg, anomalyArg, identityArg)
	if err != nil {
		t.Fatalf("insertDWD %q: %v", r.EventID, err)
	}
}

// seedHourlyRows inserts one DWD row per (hour, tokens) entry on the
// given UTC date. Thin wrapper around insertDWD that derives the
// event_time millis from "<date>T<HH>:30:00Z".
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
		insertDWD(t, db, dwdRow{
			EventID:      eventForHour(date, seatID, r.hour, i),
			OrgID:        "org1",
			SeatID:       seatID,
			EventTimeMs:  parsed.UTC().UnixMilli(),
			UsageDate:    date,
			TotalTokens:  r.tokens,
			RequestCount: r.reqs,
		})
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

// eventForHour generates a stable, per-seat-scoped event_id. Seat is
// included to keep cross-seat seeds from colliding on the
// UNIQUE (org_id, event_id) baseline constraint (the previous version
// dropped seat and worked only because the inline test schema had no
// such constraint).
func eventForHour(date, seatID string, hour, idx int) string {
	return date + "-" + seatID + "-h" + itoa(hour) + "-" + itoa(idx)
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
		{hour: 9, tokens: 50, reqs: 1}, // same hour — should aggregate
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
	insertDWD(t, db, dwdRow{
		EventID:      date + "-" + seatID + "-" + providerCode,
		OrgID:        orgID,
		SeatID:       seatID,
		ProviderCode: providerCode,
		BillingScope: billingScope,
		EventTimeMs:  parsed.UTC().UnixMilli(),
		UsageDate:    date,
		TotalTokens:  tokens,
		RequestCount: reqs,
	})
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
	insertDWD(t, db, dwdRow{
		EventID:      "ev1",
		OrgID:        "personal",
		EventTimeMs:  parsed.UTC().UnixMilli(),
		UsageDate:    date,
		TotalTokens:  42,
		RequestCount: 1,
	})
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

	insertDWD(t, db, dwdRow{
		EventID:      "boundary-ev1",
		OrgID:        "org1",
		SeatID:       "seat1",
		ProviderCode: "anthropic",
		BillingScope: "org_and_user",
		EventTimeMs:  ms,
		UsageDate:    date,
		TotalTokens:  1234,
		RequestCount: 1,
	})

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
	insertDWD(t, db, dwdRow{
		EventID:      "evA",
		OrgID:        "org1",
		SeatID:       "seat1",
		EventTimeMs:  a.UTC().UnixMilli(),
		UsageDate:    "2026-04-24",
		TotalTokens:  100,
		RequestCount: 1,
	})

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
	insertDWD(t, db, dwdRow{
		EventID:      "evB",
		OrgID:        "org1",
		SeatID:       "seat1",
		EventTimeMs:  b.UTC().UnixMilli(),
		UsageDate:    "2026-04-24",
		TotalTokens:  200,
		RequestCount: 1,
	})

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

// --- PersonalByModelTotal ---

// insertByModelDWDRow is a thin wrapper over insertDWD specialized for
// the by-model tests: an event_time / usage_date pair derived from an
// RFC3339 instant, plus the four token segments split 50/50
// input/output for non-zero totals. Centralizes the parse + math so
// the table-driven assertions below stay focused on (model, tokens)
// shape.
func insertByModelDWDRow(t *testing.T, db *shared.DB, model, seat, atUTC string, total int64) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, atUTC)
	if err != nil {
		t.Fatalf("parse %q: %v", atUTC, err)
	}
	insertDWD(t, db, dwdRow{
		EventID:      atUTC + "-" + model + "-" + seat,
		OrgID:        "org1",
		SeatID:       seat,
		Model:        model, // "<NULL>" sentinel and "" both honored by insertDWD
		EventTimeMs:  ts.UTC().UnixMilli(),
		UsageDate:    ts.UTC().Format("2006-01-02"),
		InputTokens:  total / 2,
		OutputTokens: total / 2,
		TotalTokens:  total,
		RequestCount: 1,
	})
}

// TestPersonalByModelTotal_SortsAndCoalesces covers the three behaviours
// the `/user/cost` "Usage by model" chart depends on:
//
//  1. Rows sort by SUM(total_tokens) DESC — the chart shows the
//     biggest cost driver at the top.
//  2. NULL and empty `model` collapse into a single "unknown" group —
//     no silent NULL drops that would underreport total tokens vs the
//     by-key chart on the same page.
//  3. Provider-reported model strings are kept verbatim (no
//     snapshot normalization) — `claude-sonnet-4-5-20250929` and
//     `claude-sonnet-4-6` are separate rows.
func TestPersonalByModelTotal_SortsAndCoalesces(t *testing.T) {
	db := setupUsageTestDB(t)

	// Two snapshots of Claude Sonnet — must stay separate rows.
	insertByModelDWDRow(t, db, "claude-sonnet-4-6", "seat1", "2026-04-24T10:00:00Z", 5000)
	insertByModelDWDRow(t, db, "claude-sonnet-4-5-20250929", "seat1", "2026-04-24T11:00:00Z", 3000)
	// Kimi K2 — separate provider, separate group.
	insertByModelDWDRow(t, db, "kimi-k2-0905-preview", "seat1", "2026-04-24T12:00:00Z", 2000)
	// NULL and empty model — must coalesce to "unknown".
	insertByModelDWDRow(t, db, "<NULL>", "seat1", "2026-04-24T13:00:00Z", 400)
	insertByModelDWDRow(t, db, "", "seat1", "2026-04-24T14:00:00Z", 100)
	// Other seat — must be filtered out.
	insertByModelDWDRow(t, db, "claude-sonnet-4-6", "seat2", "2026-04-24T10:30:00Z", 9999)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-01")
	end, _ := time.Parse("2006-01-02", "2026-04-30")

	got, err := repo.PersonalByModelTotal(context.Background(), QueryParams{
		SeatID:    "seat1",
		StartDate: start,
		EndDate:   end,
	})
	if err != nil {
		t.Fatalf("PersonalByModelTotal: %v", err)
	}

	if len(got) != 4 {
		t.Fatalf("want 4 rows (2 claude snapshots + kimi + unknown), got %d: %+v", len(got), got)
	}
	// (1) sort by tokens DESC.
	expectOrder := []struct {
		model  string
		tokens int64
	}{
		{"claude-sonnet-4-6", 5000},
		{"claude-sonnet-4-5-20250929", 3000},
		{"kimi-k2-0905-preview", 2000},
		{"unknown", 500}, // NULL (400) + "" (100) collapsed
	}
	for i, want := range expectOrder {
		if got[i].Model != want.model {
			t.Errorf("row %d: model = %q, want %q", i, got[i].Model, want.model)
		}
		if got[i].TotalTokens != want.tokens {
			t.Errorf("row %d (%s): total_tokens = %d, want %d", i, want.model, got[i].TotalTokens, want.tokens)
		}
	}
}

// TestPersonalByModelTotal_LimitsTop20 verifies the LIMIT 20 cap so a
// runaway model count doesn't blow the chart row layout. Seed 25
// distinct models, expect only the top 20 by tokens.
func TestPersonalByModelTotal_LimitsTop20(t *testing.T) {
	db := setupUsageTestDB(t)
	for i := 0; i < 25; i++ {
		modelName := "model-" + itoa(i)
		// Tokens descending with i so model-0 has the largest sum.
		insertByModelDWDRow(t, db, modelName, "seat1", "2026-04-24T10:00:00Z", int64((25-i)*100))
	}
	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-01")
	end, _ := time.Parse("2006-01-02", "2026-04-30")
	got, err := repo.PersonalByModelTotal(context.Background(), QueryParams{
		SeatID: "seat1", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Errorf("LIMIT 20 not applied: got %d rows", len(got))
	}
	if got[0].Model != "model-0" {
		t.Errorf("top row should be model-0 (largest tokens), got %s", got[0].Model)
	}
}

// --- AppSlug filter (Phase 4 Connected Apps, Stage B) ---

// TestPersonalTimeline_FiltersByAppSlug pins the Phase 4 Stage B
// invariant: when QueryParams.AppSlug is non-empty, PersonalTimeline
// only sums rows tagged with that app. Other apps + un-tagged rows
// (CLI / virtual-key calls) are excluded. The whole-vault rollup
// behaviour is exercised via the existing AggregatesByDay test —
// they share the same fixture seed pattern.
func TestPersonalTimeline_FiltersByAppSlug(t *testing.T) {
	db := setupUsageTestDB(t)
	// Three rows on same seat+day: app-A, app-B, no-app (CLI). Same
	// totals so a leak would compound and be obvious in the assertion.
	mustInsertDWDWithApp(t, db, "2026-04-20", "seat1", "org1", "app-A", 100)
	mustInsertDWDWithApp(t, db, "2026-04-20", "seat1", "org1", "app-B", 100)
	mustInsertDWDWithApp(t, db, "2026-04-20", "seat1", "org1", "", 100) // no app — CLI / VK call

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-01")
	end, _ := time.Parse("2006-01-02", "2026-04-30")

	// No filter → all three rows sum to 300.
	all, err := repo.PersonalTimeline(context.Background(), QueryParams{
		SeatID: "seat1", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].TotalTokens != 300 {
		t.Fatalf("baseline (no filter): want 300 tokens / 1 day, got %+v", all)
	}

	// Filter app-A → only 100.
	onlyA, err := repo.PersonalTimeline(context.Background(), QueryParams{
		SeatID: "seat1", StartDate: start, EndDate: end, AppSlug: "app-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyA) != 1 || onlyA[0].TotalTokens != 100 {
		t.Fatalf("AppSlug=app-A filter leak: want 100 tokens / 1 day, got %+v", onlyA)
	}

	// Filter app-Missing → zero (not an error — just no matching rows).
	none, err := repo.PersonalTimeline(context.Background(), QueryParams{
		SeatID: "seat1", StartDate: start, EndDate: end, AppSlug: "app-Missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown AppSlug should return no rows, got %+v", none)
	}
}

// TestPersonalByModelTotal_FiltersByAppSlug — same invariant for the
// by-model aggregate. Apps Detail page needs per-app model breakdown.
func TestPersonalByModelTotal_FiltersByAppSlug(t *testing.T) {
	db := setupUsageTestDB(t)
	insertDWD(t, db, dwdRow{
		EventID: "evt-A", OrgID: "org1", SeatID: "seat1",
		ProviderCode: "anthropic", Model: "claude-3-7",
		EventTimeMs: mustParseDateMs(t, "2026-04-25"), UsageDate: "2026-04-25",
		TotalTokens: 500, RequestCount: 1, AppSlug: "app-A",
	})
	insertDWD(t, db, dwdRow{
		EventID: "evt-B", OrgID: "org1", SeatID: "seat1",
		ProviderCode: "anthropic", Model: "claude-3-7",
		EventTimeMs: mustParseDateMs(t, "2026-04-25"), UsageDate: "2026-04-25",
		TotalTokens: 800, RequestCount: 1, AppSlug: "app-B",
	})
	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-04-01")
	end, _ := time.Parse("2006-01-02", "2026-04-30")

	onlyA, err := repo.PersonalByModelTotal(context.Background(), QueryParams{
		SeatID: "seat1", StartDate: start, EndDate: end, AppSlug: "app-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyA) != 1 || onlyA[0].Model != "claude-3-7" || onlyA[0].TotalTokens != 500 {
		t.Fatalf("AppSlug=app-A filter leak (by-model): %+v", onlyA)
	}
}

// mustInsertDWDWithApp is a one-row helper for the AppSlug timeline
// test. Wraps insertDWD with a minimal set of fields — all the tests
// in this file insert via insertDWD, so the helper just sets the
// AppSlug-relevant defaults.
func mustInsertDWDWithApp(t *testing.T, db *shared.DB, date, seatID, orgID, appSlug string, tokens int64) {
	t.Helper()
	insertDWD(t, db, dwdRow{
		EventID:      "evt-" + appSlug + "-" + date,
		OrgID:        orgID,
		SeatID:       seatID,
		ProviderCode: "anthropic",
		Model:        "claude-3-7",
		EventTimeMs:  mustParseDateMs(t, date),
		UsageDate:    date,
		TotalTokens:  tokens,
		RequestCount: 1,
		AppSlug:      appSlug,
	})
}

func mustParseDateMs(t *testing.T, ymd string) int64 {
	t.Helper()
	tt, err := time.Parse("2006-01-02", ymd)
	if err != nil {
		t.Fatal(err)
	}
	return tt.UnixMilli()
}

// insertODSWithIdentity seeds a minimal usage_event_ods row that links a
// virtual_key_id to an oauth_identity for the LEFT JOIN inside
// PersonalByKeyTotal. Only the fields the subquery reads are populated;
// the NOT NULL columns without DEFAULTs (request_status, raw_event_json)
// get sentinel placeholders since the query never reads them.
func insertODSWithIdentity(t *testing.T, db *shared.DB, eventID, virtualKeyID, identity string, eventTimeMs int64) {
	t.Helper()
	_, err := db.DB.Exec(`
		INSERT INTO usage_event_ods (
			event_id, event_time, occurred_at, org_id,
			virtual_key_id, oauth_identity,
			request_status, raw_event_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, eventTimeMs, eventTimeMs, "personal",
		virtualKeyID, identity,
		"success", "{}")
	if err != nil {
		t.Fatalf("insertODSWithIdentity %q: %v", eventID, err)
	}
}

// TestPersonalByKeyTotal_CollapsesOAuthSessionsBySameApp covers the
// usage-by-key duplicate-rows bugfix (2026-05-26).
//
// Setup: one email (alice@example.com) with three distinct OAuth
// virtual_key_ids — modeling the same user who logged in from three
// devices / refreshed sessions. All three sessions carry the same
// app_slug ("claude-code") because they all came from the same client.
//
// Expectation: the new GROUP BY collapses them into ONE row whose
// total_tokens is the sum (100+200+50=350). Pre-fix this returned
// three rows that looked identical in the UI but split the totals.
//
// See:
//
//	workflow/CI/bugfix/20260526-usage-by-key-duplicate-rows-by-app-attribution.md
func TestPersonalByKeyTotal_CollapsesOAuthSessionsBySameApp(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-05-20"
	ms := mustParseDateMs(t, date)
	identity := "alice@example.com"

	sessions := []struct {
		vk     string
		tokens int64
	}{
		{"oauth:session_aaa111", 100},
		{"oauth:session_bbb222", 200},
		{"oauth:session_ccc333", 50},
	}
	for _, s := range sessions {
		insertDWD(t, db, dwdRow{
			EventID: "dwd-" + s.vk, OrgID: "personal",
			VirtualKeyID: s.vk,
			ProviderCode: "anthropic", Model: "claude-3-7",
			EventTimeMs: ms, UsageDate: date,
			TotalTokens: s.tokens, RequestCount: 1,
			AppSlug: "claude-code",
		})
		insertODSWithIdentity(t, db, "ods-"+s.vk, s.vk, identity, ms)
	}

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-05-01")
	end, _ := time.Parse("2006-01-02", "2026-05-31")
	rows, err := repo.PersonalByKeyTotal(context.Background(), QueryParams{
		OrgID: "personal", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 collapsed row, got %d: %+v", len(rows), rows)
	}
	if rows[0].Identity != identity {
		t.Errorf("identity want %q, got %q", identity, rows[0].Identity)
	}
	if rows[0].AppSlug != "claude-code" {
		t.Errorf("app_slug want claude-code, got %q", rows[0].AppSlug)
	}
	if rows[0].TotalTokens != 350 {
		t.Errorf("total_tokens want 350 (sum), got %d", rows[0].TotalTokens)
	}
}

// TestPersonalByKeyTotal_SplitsOAuthSessionsByDifferentApps verifies
// the second axis of the new aggregation: same email, different
// clients (UA-derived app_slug) → distinct rows.
//
// This is the "FreySilvaqzs@... three rows but three different
// clients" scenario from the bugfix discussion. Each client gets its
// own bar in the dashboard so users can tell where the spend came from.
func TestPersonalByKeyTotal_SplitsOAuthSessionsByDifferentApps(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-05-20"
	ms := mustParseDateMs(t, date)
	identity := "alice@example.com"

	sessions := []struct {
		vk      string
		tokens  int64
		appSlug string
	}{
		{"oauth:session_cc1", 500, "claude-code"},
		{"oauth:session_cur1", 300, "cursor"},
		{"oauth:session_unk1", 100, "unknown-app"},
	}
	for _, s := range sessions {
		insertDWD(t, db, dwdRow{
			EventID: "dwd-" + s.vk, OrgID: "personal",
			VirtualKeyID: s.vk,
			ProviderCode: "anthropic", Model: "claude-3-7",
			EventTimeMs: ms, UsageDate: date,
			TotalTokens: s.tokens, RequestCount: 1,
			AppSlug: s.appSlug,
		})
		insertODSWithIdentity(t, db, "ods-"+s.vk, s.vk, identity, ms)
	}

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-05-01")
	end, _ := time.Parse("2006-01-02", "2026-05-31")
	rows, err := repo.PersonalByKeyTotal(context.Background(), QueryParams{
		OrgID: "personal", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (one per app), got %d: %+v", len(rows), rows)
	}
	seen := map[string]int64{}
	for _, r := range rows {
		if r.Identity != identity {
			t.Errorf("row %+v: identity want %q", r, identity)
		}
		seen[r.AppSlug] = r.TotalTokens
	}
	if seen["claude-code"] != 500 || seen["cursor"] != 300 || seen["unknown-app"] != 100 {
		t.Errorf("per-app totals wrong: %+v", seen)
	}
}

// TestPersonalByKeyTotal_NonOAuthRowsUnaffected pins the non-OAuth
// branch: rows with empty oauth_identity (personal CLI keys, team
// keys, no-app legacy events) still group by virtual_key_id as before
// — the new COALESCE(NULLIF(identity,”), vk_id) reduces to vk_id when
// identity is empty.
func TestPersonalByKeyTotal_NonOAuthRowsUnaffected(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-05-20"
	ms := mustParseDateMs(t, date)

	// Two distinct personal vk_ids — no oauth_identity in ODS so the
	// LEFT JOIN yields NULL identity for both → group falls back to
	// vk_id. Both have empty app_slug (legacy events).
	insertDWD(t, db, dwdRow{
		EventID: "dwd-pers-1", OrgID: "personal",
		VirtualKeyID: "personal:alice-key",
		ProviderCode: "anthropic", Model: "claude-3-7",
		EventTimeMs: ms, UsageDate: date,
		TotalTokens: 100, RequestCount: 1,
	})
	insertDWD(t, db, dwdRow{
		EventID: "dwd-pers-2", OrgID: "personal",
		VirtualKeyID: "personal:bob-key",
		ProviderCode: "anthropic", Model: "claude-3-7",
		EventTimeMs: ms, UsageDate: date,
		TotalTokens: 200, RequestCount: 1,
	})

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-05-01")
	end, _ := time.Parse("2006-01-02", "2026-05-31")
	rows, err := repo.PersonalByKeyTotal(context.Background(), QueryParams{
		OrgID: "personal", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 distinct vk rows (no collapse without identity), got %d: %+v", len(rows), rows)
	}
}

// --- PersonalBySessionTotal (v1.0.0-rc.6) ---

// insertDWDWithSession seeds one DWD row with explicit session_id.
// Helper for the by-session aggregation tests below.
func insertDWDWithSession(t *testing.T, db *shared.DB, eventID, sessionID, vkID string, ms int64, tokens int64) {
	t.Helper()
	insertDWD(t, db, dwdRow{
		EventID:      eventID,
		OrgID:        "personal",
		VirtualKeyID: vkID,
		ProviderCode: "anthropic",
		Model:        "claude-3-7",
		EventTimeMs:  ms,
		UsageDate:    "2026-05-20",
		TotalTokens:  tokens,
		RequestCount: 1,
	})
	// Need to write session_id via direct UPDATE — insertDWD helper
	// doesn't take session_id as a parameter, but the column was added
	// in postBaseline migration so the UPDATE succeeds.
	_, err := db.DB.Exec(`UPDATE usage_fact_dwd SET session_id = ? WHERE event_id = ?`, sessionID, eventID)
	if err != nil {
		t.Fatalf("update session_id for %q: %v", eventID, err)
	}
}

// TestPersonalBySessionTotal_GroupsBySessionAndSortsByTokens covers the
// happy path: distinct sessions get distinct rows, sorted by total_tokens
// DESC, and the LIMIT param is respected.
func TestPersonalBySessionTotal_GroupsBySessionAndSortsByTokens(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-05-20"
	ms := mustParseDateMs(t, date)
	insertDWDWithSession(t, db, "e1", "session-big", "personal:k1", ms, 1000)
	insertDWDWithSession(t, db, "e2", "session-mid", "personal:k2", ms, 500)
	insertDWDWithSession(t, db, "e3", "session-small", "personal:k3", ms, 100)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-05-01")
	end, _ := time.Parse("2006-01-02", "2026-05-31")
	rows, err := repo.PersonalBySessionTotal(context.Background(), QueryParams{
		OrgID: "personal", StartDate: start, EndDate: end, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d: %+v", len(rows), rows)
	}
	wantOrder := []string{"session-big", "session-mid", "session-small"}
	for i, want := range wantOrder {
		if rows[i].SessionID != want {
			t.Errorf("rows[%d].SessionID = %q, want %q (sort by total_tokens DESC)", i, rows[i].SessionID, want)
		}
	}
}

// TestPersonalBySessionTotal_NoSessionBucket pins the "(no session)"
// aggregation contract: rows with NULL or ” session_id collapse into
// a single bucket the frontend renders as "(no session)". Confirms
// users can see how much of their traffic lacks the dimension.
func TestPersonalBySessionTotal_NoSessionBucket(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-05-20"
	ms := mustParseDateMs(t, date)
	// 2 rows with NULL session_id (leave session unset)
	insertDWD(t, db, dwdRow{EventID: "n1", OrgID: "personal", VirtualKeyID: "personal:k1", ProviderCode: "anthropic", Model: "c", EventTimeMs: ms, UsageDate: date, TotalTokens: 100, RequestCount: 1})
	insertDWD(t, db, dwdRow{EventID: "n2", OrgID: "personal", VirtualKeyID: "personal:k2", ProviderCode: "anthropic", Model: "c", EventTimeMs: ms, UsageDate: date, TotalTokens: 200, RequestCount: 1})
	// 1 row with explicit empty session_id (NOT NULL but empty)
	insertDWD(t, db, dwdRow{EventID: "n3", OrgID: "personal", VirtualKeyID: "personal:k3", ProviderCode: "anthropic", Model: "c", EventTimeMs: ms, UsageDate: date, TotalTokens: 50, RequestCount: 1})
	_, _ = db.DB.Exec(`UPDATE usage_fact_dwd SET session_id = '' WHERE event_id = 'n3'`)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-05-01")
	end, _ := time.Parse("2006-01-02", "2026-05-31")
	rows, err := repo.PersonalBySessionTotal(context.Background(), QueryParams{
		OrgID: "personal", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 collapsed row (no session bucket), got %d: %+v", len(rows), rows)
	}
	if rows[0].SessionID != "" {
		t.Errorf("collapsed bucket SessionID want empty, got %q", rows[0].SessionID)
	}
	if rows[0].TotalTokens != 350 {
		t.Errorf("collapsed bucket total tokens want 350 (100+200+50), got %d", rows[0].TotalTokens)
	}
}

// TestPersonalBySessionTotal_IgnoresSessionIDFilter pins the design
// contract from §5.3: passing SessionID on the by-session endpoint
// does NOT shrink the result to one row. Selecting a session in the
// UI must keep the Top N visible so users can switch to another
// session without resetting state.
func TestPersonalBySessionTotal_IgnoresSessionIDFilter(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-05-20"
	ms := mustParseDateMs(t, date)
	insertDWDWithSession(t, db, "e1", "session-A", "personal:k", ms, 100)
	insertDWDWithSession(t, db, "e2", "session-B", "personal:k", ms, 200)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-05-01")
	end, _ := time.Parse("2006-01-02", "2026-05-31")
	rows, err := repo.PersonalBySessionTotal(context.Background(), QueryParams{
		OrgID: "personal", StartDate: start, EndDate: end,
		SessionID: "session-A", // intentionally ignored
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("SessionID filter must be ignored: want 2 rows, got %d: %+v", len(rows), rows)
	}
}

// TestPersonalByKeyTotal_FiltersBySession is the read-side companion
// to the new SessionID filter on by-key. Confirms the SQL WHERE
// clause narrows correctly without breaking app_slug grouping.
func TestPersonalByKeyTotal_FiltersBySession(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-05-20"
	ms := mustParseDateMs(t, date)
	insertDWDWithSession(t, db, "e1", "session-target", "personal:k1", ms, 500)
	insertDWDWithSession(t, db, "e2", "session-other", "personal:k2", ms, 999)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-05-01")
	end, _ := time.Parse("2006-01-02", "2026-05-31")
	rows, err := repo.PersonalByKeyTotal(context.Background(), QueryParams{
		OrgID: "personal", StartDate: start, EndDate: end,
		SessionID: "session-target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TotalTokens != 500 {
		t.Fatalf("SessionID filter leak: want 1 row of 500 tokens, got %+v", rows)
	}
}

// TestPersonalByModelTotal_FiltersBySession mirrors the above for the
// by-model endpoint. Both endpoints share the sessionIDFilter helper
// so the contract is identical.
func TestPersonalByModelTotal_FiltersBySession(t *testing.T) {
	db := setupUsageTestDB(t)
	date := "2026-05-20"
	ms := mustParseDateMs(t, date)
	insertDWDWithSession(t, db, "e1", "session-target", "personal:k1", ms, 500)
	insertDWDWithSession(t, db, "e2", "session-other", "personal:k2", ms, 999)

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-05-01")
	end, _ := time.Parse("2006-01-02", "2026-05-31")
	rows, err := repo.PersonalByModelTotal(context.Background(), QueryParams{
		OrgID: "personal", StartDate: start, EndDate: end,
		SessionID: "session-target",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both events use Model "claude-3-7", so filter narrows to one model row of 500 tokens.
	if len(rows) != 1 || rows[0].TotalTokens != 500 {
		t.Fatalf("SessionID filter leak: want 1 row of 500 tokens, got %+v", rows)
	}
}

// 🔴 "Which upstreams did we switch to lately, and why" (openspec change
// `aliyun-aigw-p0-upstream-fallback`, task 4.5b).
//
// The console is forbidden from seeing the live cooldown table — it never leaves
// the developer's machine (I23) — so this aggregate is the only form the question
// can take. Two things have to be right or it misleads in opposite directions:
// a row with NULL attempt must NOT be counted as a switch (it predates the
// field), and a row with attempt=1 must NOT be counted either (the primary served
// it, which is the healthy case and by far the most common).
func TestMasterUpstreamStepArounds(t *testing.T) {
	db := setupUsageTestDB(t)
	day, _ := time.Parse("2006-01-02", "2026-07-20")
	base, _ := time.Parse(time.RFC3339, "2026-07-20T10:00:00Z")
	baseMs := base.UnixMilli()

	seed := func(id, provider string, attempt any, reason any, offsetMin int64) {
		insertDWD(t, db, dwdRow{
			EventID: id, OrgID: "org1", SeatID: "seat1",
			EventTimeMs: baseMs + offsetMin*60_000, UsageDate: "2026-07-20",
			ProviderCode: provider, ProtocolType: "anthropic", Model: "claude",
			RequestCount: 1, TotalTokens: 10, BillingScope: "org_only", UserUsageScope: "none",
		})
		if _, err := db.DB.Exec(
			`UPDATE usage_fact_dwd SET fallback_attempt = ?, fallback_reason = ? WHERE event_id = ?`,
			attempt, reason, id); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
	}

	seed("e-primary", "anthropic", 1, nil, 0)         // primary served it — not a switch
	seed("e-legacy", "anthropic", nil, nil, 1)        // written before the field existed
	seed("e-switch-1", "zhipu", 2, "UPSTREAM_5XX", 2) // switched
	seed("e-switch-2", "zhipu", 2, "UPSTREAM_5XX", 3)
	seed("e-switch-3", "zhipu", 3, "UPSTREAM_TIMEOUT", 4)

	repo := NewSQLRepository(db)
	got, err := repo.MasterUpstreamStepArounds(context.Background(), QueryParams{
		OrgID: "org1", StartDate: day, EndDate: day,
	})
	if err != nil {
		t.Fatalf("MasterUpstreamStepArounds: %v", err)
	}

	total := int64(0)
	for _, s := range got {
		total += s.Switches
		if s.ProviderCode == "anthropic" {
			t.Errorf("the primary-served and legacy rows were counted as switches: %+v", s)
		}
	}
	if total != 3 {
		t.Fatalf("counted %d switches across %+v, want 3", total, got)
	}

	byReason := map[string]int64{}
	var last int64
	for _, s := range got {
		byReason[s.Reason] = s.Switches
		if s.LastAt > last {
			last = s.LastAt
		}
	}
	if byReason["UPSTREAM_5XX"] != 2 || byReason["UPSTREAM_TIMEOUT"] != 1 {
		t.Errorf("per-reason counts = %v, want 5XX:2 TIMEOUT:1", byReason)
	}
	if last != baseMs+4*60_000 {
		t.Errorf("last_at = %d, want the most recent switch (%d)", last, baseMs+4*60_000)
	}
}

// group_by=provider (2026-08-18, tray route↔usage linkage variant B): one row
// per (hour, provider_code), so a client can build per-provider or per-family
// series without a second query shape. The fence pins three things at once:
// the grouped rows carry the dimension, the SAME params without the flag keep
// the exact pre-existing one-row-per-hour shape (additive contract), and the
// grouped rows sum to the ungrouped ones (no traffic invented or dropped).
func TestPersonalHourlyTimeline_GroupByProvider(t *testing.T) {
	db := setupUsageTestDB(t)

	utcInstant, _ := time.Parse(time.RFC3339, "2026-04-24T10:30:00Z")
	ms := utcInstant.UTC().UnixMilli()
	const date = "2026-04-24"
	// Two providers in the SAME hour — the one case a per-hour rollup cannot
	// represent and the grouped mode exists for.
	insertDWD(t, db, dwdRow{
		EventID: "grp-ev1", OrgID: "org1", SeatID: "seat1",
		ProviderCode: "moonshot", BillingScope: "org_and_user",
		EventTimeMs: ms, UsageDate: date, TotalTokens: 400, RequestCount: 4,
	})
	insertDWD(t, db, dwdRow{
		EventID: "grp-ev2", OrgID: "org1", SeatID: "seat1",
		ProviderCode: "anthropic", BillingScope: "org_and_user",
		EventTimeMs: ms, UsageDate: date, TotalTokens: 600, RequestCount: 6,
	})

	repo := NewSQLRepository(db)
	day, _ := time.Parse("2006-01-02", date)

	grouped, err := repo.PersonalHourlyTimeline(context.Background(), QueryParams{
		SeatID: "seat1", StartDate: day, GroupByProvider: true,
	})
	if err != nil {
		t.Fatalf("grouped: %v", err)
	}
	if len(grouped) != 2 {
		t.Fatalf("want one row per (hour, provider), got %+v", grouped)
	}
	byProv := map[string]HourlyPoint{}
	for _, p := range grouped {
		if p.Hour != 10 {
			t.Errorf("row hour = %d, want 10: %+v", p.Hour, p)
		}
		byProv[p.ProviderCode] = p
	}
	if byProv["moonshot"].TotalTokens != 400 || byProv["anthropic"].TotalTokens != 600 {
		t.Errorf("grouped split wrong: %+v", byProv)
	}

	ungrouped, err := repo.PersonalHourlyTimeline(context.Background(), QueryParams{
		SeatID: "seat1", StartDate: day,
	})
	if err != nil {
		t.Fatalf("ungrouped: %v", err)
	}
	if len(ungrouped) != 1 || ungrouped[0].ProviderCode != "" || ungrouped[0].TotalTokens != 1000 {
		t.Errorf("ungrouped mode must stay one bare row per hour summing everything: %+v", ungrouped)
	}
}
