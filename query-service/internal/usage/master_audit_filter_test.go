package usage

import (
	"context"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

// 20260729 用量审计页自由筛选 — fence tests for the master audit filter
// push-down (masterAuditWhere dimension registry).
//
// Four rows, one org, one day, on the REAL baseline schema, differing in every
// filter dimension so each dimension provably narrows on its own column:
//
//	ev-a  seat1 credA anthropic claude-x  exact    priced   vk1 anthropic_native
//	ev-b  seat2 credB openai    gpt-5.6   invalid  unpriced vk2 openai_compatible (anomaly token_spike)
//	ev-c  seat1 credB anthropic claude-x  partial  unpriced vk3 anthropic_native
//	ev-p  probe row (non_generation)      — must NEVER appear, filtered or not
//
// Pinned rules:
//   - no filters → all three audit rows (backward compat: an old FE that sends
//     no params sees exactly the pre-filter behaviour)
//   - each dimension narrows independently; dimensions AND-combine
//   - detail and export share masterAuditWhere → identical row sets for the
//     same params (the "screen scope == CSV scope" invariant)
func TestMasterAuditFilters(t *testing.T) {
	db := setupUsageTestDB(t)
	seedOrgSeatsFixture(t, db)

	day := "2026-07-20"
	ts := msAt("2026-07-20T10:00:00Z")
	priced := 0.5
	insertDWD(t, db, dwdRow{
		EventID: "ev-a", OrgID: "org1", SeatID: "seat1", CredentialID: "credA",
		ProviderCode: "anthropic", ProtocolType: "anthropic_native",
		Model: "claude-x", VirtualKeyID: "vk1", QualityStatus: "exact",
		BillableAmount: &priced,
		EventTimeMs:    ts, UsageDate: day, TotalTokens: 100,
	})
	insertDWD(t, db, dwdRow{
		EventID: "ev-b", OrgID: "org1", SeatID: "seat2", CredentialID: "credB",
		ProviderCode: "openai", ProtocolType: "openai_compatible",
		Model: "gpt-5.6", VirtualKeyID: "vk2", QualityStatus: "invalid",
		AnomalyType: "token_spike",
		EventTimeMs: ts, UsageDate: day, TotalTokens: 200,
	})
	insertDWD(t, db, dwdRow{
		EventID: "ev-c", OrgID: "org1", SeatID: "seat1", CredentialID: "credB",
		ProviderCode: "anthropic", ProtocolType: "anthropic_native",
		Model: "claude-x", VirtualKeyID: "vk3", QualityStatus: "partial",
		EventTimeMs: ts, UsageDate: day, TotalTokens: 300,
	})
	insertDWD(t, db, dwdRow{
		EventID: "ev-p", OrgID: "org1", SeatID: "seat1", CredentialID: "credA",
		ProviderCode: "anthropic", ProtocolType: "anthropic_native",
		Model: "claude-x", VirtualKeyID: "vk1",
		UserUsageScope: "non_generation",
		EventTimeMs:    ts, UsageDate: day, TotalTokens: 0,
	})

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", day)
	base := QueryParams{OrgID: "org1", StartDate: start, EndDate: start, Limit: 100}

	cases := []struct {
		name string
		mut  func(*QueryParams)
		want []string
	}{
		{"no filters (backward compat)", func(p *QueryParams) {}, []string{"ev-a", "ev-b", "ev-c"}},
		{"seat", func(p *QueryParams) { p.SeatID = "seat1" }, []string{"ev-a", "ev-c"}},
		{"credential", func(p *QueryParams) { p.CredentialID = "credB" }, []string{"ev-b", "ev-c"}},
		{"provider", func(p *QueryParams) { p.ProviderCode = "openai" }, []string{"ev-b"}},
		{"model", func(p *QueryParams) { p.Model = "claude-x" }, []string{"ev-a", "ev-c"}},
		{"quality", func(p *QueryParams) { p.QualityStatus = "invalid" }, []string{"ev-b"}},
		{"virtual key", func(p *QueryParams) { p.VirtualKeyID = "vk1" }, []string{"ev-a"}},
		{"protocol", func(p *QueryParams) { p.Protocol = "openai_compatible" }, []string{"ev-b"}},
		{"anomaly", func(p *QueryParams) { p.AnomalyType = "token_spike" }, []string{"ev-b"}},
		{"billing priced", func(p *QueryParams) { p.Billing = "priced" }, []string{"ev-a"}},
		{"billing unpriced", func(p *QueryParams) { p.Billing = "unpriced" }, []string{"ev-b", "ev-c"}},
		{"AND combine seat+unpriced", func(p *QueryParams) { p.SeatID = "seat1"; p.Billing = "unpriced" }, []string{"ev-c"}},
		{"AND combine provider+model+quality", func(p *QueryParams) {
			p.ProviderCode = "anthropic"
			p.Model = "claude-x"
			p.QualityStatus = "exact"
		}, []string{"ev-a"}},
		{"filter matching nothing", func(p *QueryParams) { p.SeatID = "seat-none" }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mut(&p)
			detail, err := repo.MasterUsageDetail(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			assertEventIDs(t, "detail", detail, tc.want)

			// Same params through export MUST yield the same row set —
			// detail and export share masterAuditWhere by construction, and
			// this fence keeps a future refactor from splitting them.
			var exported []MasterUsageAuditRow
			if err := repo.StreamMasterUsageExport(context.Background(), p, func(a *MasterUsageAuditRow) error {
				exported = append(exported, *a)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			assertEventIDs(t, "export", exported, tc.want)
		})
	}
}

func assertEventIDs(t *testing.T, label string, rows []MasterUsageAuditRow, want []string) {
	t.Helper()
	got := map[string]bool{}
	for _, r := range rows {
		if r.EventID == "ev-p" {
			t.Errorf("%s: non_generation probe row leaked into audit result", label)
		}
		got[r.EventID] = true
	}
	if len(rows) != len(want) {
		t.Fatalf("%s: got %d rows (%v), want %d (%v)", label, len(rows), got, len(want), want)
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("%s: missing expected row %s (got %v)", label, id, got)
		}
	}
}

// seedOrgSeatsFixture mirrors the org_seats control-plane DDL the audit JOIN
// needs — same convention as scope_filter_test.go (see the DDL provenance
// comment there):
//
//	aikey-control-master/service/migrations/001_baseline_schema.sql (org_seats)
//	aikey-config-tool/pkg/dbmigrate/versions_master/v1_0_1_alpha1_digital_employees.go (alias)
func seedOrgSeatsFixture(t *testing.T, db *shared.DB) {
	t.Helper()
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
		`INSERT INTO org_seats (seat_id, org_id, invited_email, alias) VALUES
			('seat1','org1','a@corp','Alice'), ('seat2','org1','b@corp','Bob')`,
	); err != nil {
		t.Fatalf("seed org_seats: %v", err)
	}
}
