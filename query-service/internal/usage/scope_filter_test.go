package usage

import (
	"context"
	"testing"
	"time"
)

// 2026-07-15 非生成流量不进用量审计与统计 — scope-filter fence tests.
//
// Three rows, one org/seat/day, on the REAL baseline schema:
//   - normal          (tokens=1000)  a real generation call
//   - non_generation  (tokens=0)     a probe/poll (GET /v1/models shape)
//   - excluded        (tokens=77)    ownership-unverifiable (pending review)
//
// Rules pinned (see repository_sql.go scopeStatsAnd/scopeAuditAnd):
//   - stats  (timeline / rankings): normal ONLY  → 1000 tokens, 1 request
//   - audit  (master detail/export): normal + excluded, never non_generation
//
// A regression that blends the two rules (e.g. audit accidentally using the
// stats rule) hides pending_review anomalies from auditors — that's why both
// directions are asserted, not just the probe exclusion.
func TestScopeFilters_StatsAndAuditRules(t *testing.T) {
	db := setupUsageTestDB(t)
	// masterAuditFrom LEFT JOINs org_seats (control-plane table, not in the
	// baseline data component). Mirror the real DDL by hand, same convention
	// as setupUsageTestDB's post-baseline block:
	//   aikey-control-master/service/migrations/001_baseline_schema.sql (org_seats)
	//   aikey-config-tool/pkg/dbmigrate/versions_master/v1_0_1_alpha1_digital_employees.go (alias)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS org_seats (
			seat_id       TEXT NOT NULL PRIMARY KEY,
			org_id        TEXT NOT NULL,
			account_id    TEXT,
			invited_email TEXT NOT NULL,
			seat_status   TEXT NOT NULL DEFAULT 'pending_claim',
			claimed_at    TIMESTAMPTZ,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT (datetime('now')),
			CONSTRAINT uq_org_seats_org_email UNIQUE (org_id, invited_email)
		)`,
		`ALTER TABLE org_seats ADD COLUMN seat_type TEXT NOT NULL DEFAULT 'human'`,
		`ALTER TABLE org_seats ADD COLUMN alias TEXT`,
	} {
		if _, err := db.DB.Exec(stmt); err != nil {
			t.Fatalf("org_seats fixture DDL: %v", err)
		}
	}
	if _, err := db.DB.Exec(
		`INSERT INTO org_seats (seat_id, org_id, invited_email, alias) VALUES ('seat1','org1','a@corp','Alice')`,
	); err != nil {
		t.Fatalf("seed org_seats: %v", err)
	}
	insertDWD(t, db, dwdRow{
		EventID: "ev-normal", OrgID: "org1", SeatID: "seat1",
		ProviderCode: "openai", ProtocolType: "openai_compatible",
		Model: "gpt-5.6", EventTimeMs: msAt("2026-07-14T10:00:00Z"),
		UsageDate: "2026-07-14", TotalTokens: 1000, RequestCount: 1,
		BillingScope: "org_and_user",
	})
	insertDWD(t, db, dwdRow{
		EventID: "ev-probe", OrgID: "org1", SeatID: "seat1",
		ProviderCode: "openai", ProtocolType: "openai_compatible",
		Model: "<NULL>", EventTimeMs: msAt("2026-07-14T10:03:00Z"),
		UsageDate: "2026-07-14", TotalTokens: 0, RequestCount: 1,
		BillingScope: "org_and_user", UserUsageScope: "non_generation",
	})
	insertDWD(t, db, dwdRow{
		EventID: "ev-excluded", OrgID: "org1", SeatID: "seat1",
		ProviderCode: "openai", ProtocolType: "openai_compatible",
		Model: "gpt-5.6", EventTimeMs: msAt("2026-07-14T10:06:00Z"),
		UsageDate: "2026-07-14", TotalTokens: 77, RequestCount: 1,
		BillingScope: "org_only", UserUsageScope: "excluded",
	})

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-07-01")
	end, _ := time.Parse("2006-01-02", "2026-07-31")

	// --- stats rule: normal only ---
	points, err := repo.PersonalTimeline(context.Background(), QueryParams{
		SeatID: "seat1", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].TotalTokens != 1000 || points[0].RequestCount != 1 {
		t.Errorf("stats rule: want single day 1000 tok / 1 req (normal only), got %+v", points)
	}

	ranking, err := repo.MasterUserRanking(context.Background(), QueryParams{
		OrgID: "org1", StartDate: start, EndDate: end, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ranking) != 1 || ranking[0].TotalTokens != 1000 || ranking[0].RequestCount != 1 {
		t.Errorf("ranking stats rule: want 1000 tok / 1 req, got %+v", ranking)
	}

	// --- org billing charts: exclude probes, keep org_only excluded rows ---
	tl, err := repo.MasterTimeline(context.Background(), QueryParams{
		OrgID: "org1", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tl) != 1 || tl[0].TotalTokens != 1077 || tl[0].RequestCount != 2 {
		t.Errorf("master timeline: want 1077 tok / 2 req (normal+excluded, no probe), got %+v", tl)
	}

	// --- audit rule: normal + excluded visible, probe hidden ---
	audit, err := repo.MasterUsageDetail(context.Background(), QueryParams{
		OrgID: "org1", StartDate: start, EndDate: end, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range audit {
		got[row.EventID] = true
	}
	if !got["ev-normal"] || !got["ev-excluded"] {
		t.Errorf("audit rule: normal + excluded must be visible, got %v", got)
	}
	if got["ev-probe"] {
		t.Error("audit rule: non_generation probe row leaked into usage-audit")
	}
	if len(audit) != 2 {
		t.Errorf("audit rows = %d, want 2: %v", len(audit), got)
	}

	// --- export mirrors the audit rule ---
	var exported []string
	err = repo.StreamMasterUsageExport(context.Background(), QueryParams{
		OrgID: "org1", StartDate: start, EndDate: end,
	}, func(r *MasterUsageAuditRow) error {
		exported = append(exported, r.EventID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(exported) != 2 {
		t.Errorf("export rows = %d, want 2 (no probe): %v", len(exported), exported)
	}
}

// msAt parses an RFC3339 instant into epoch millis for seeding.
func msAt(iso string) int64 {
	ts, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		panic(err)
	}
	return ts.UnixMilli()
}
