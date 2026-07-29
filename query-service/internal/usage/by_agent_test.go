package usage

import (
	"context"
	"testing"
	"time"
)

// 2026-07-17 usage-ledger "Usage By Agent" breakdown — authorization + grouping
// fence test (requirement 2026-07-17-usage-ledger-by-agent-breakdown).
//
// PersonalByAgentTotal must return exactly the caller's OWN seat row plus the
// seats it owns (org_seats.parent_seat_id = caller) — its Agents (数字员工) —
// and NOTHING else. The most important assertion is negative: a stranger's
// agent (owned by a different parent) must NOT appear. A regression that drops
// or widens the `s.parent_seat_id = ?` scope leaks another user's usage, which
// is both a privacy breach and wrong billing attribution — so this test seeds
// a third seat owned by someone else and asserts it's absent.
//
// Fixture uses the REAL org_seats shape: seat_type (v1.0.0 baseline /
// v1.0.1-alpha.1) + parent_seat_id (v1.0.1-alpha.5). Mirrors the by-hand DDL
// convention in scope_filter_test.go.
func TestPersonalByAgentTotal_ScopeAndGrouping(t *testing.T) {
	db := setupUsageTestDB(t)
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
		`ALTER TABLE org_seats ADD COLUMN parent_seat_id TEXT`,
	} {
		if _, err := db.DB.Exec(stmt); err != nil {
			t.Fatalf("org_seats fixture DDL: %v", err)
		}
	}
	// seat1 = caller (human). agent1 = caller's Agent. agent2 = a STRANGER's
	// Agent (owned by seat-other), same org — must never surface for seat1.
	for _, stmt := range []string{
		`INSERT INTO org_seats (seat_id, org_id, invited_email, seat_type, alias) VALUES ('seat1','org1','me@corp','human','Me')`,
		`INSERT INTO org_seats (seat_id, org_id, invited_email, seat_type, alias, parent_seat_id) VALUES ('agent1','org1','bot1@corp','digital_employee','Bot One','seat1')`,
		`INSERT INTO org_seats (seat_id, org_id, invited_email, seat_type, alias, parent_seat_id) VALUES ('agent2','org1','bot2@corp','digital_employee','Stranger Bot','seat-other')`,
	} {
		if _, err := db.DB.Exec(stmt); err != nil {
			t.Fatalf("seed org_seats: %v", err)
		}
	}

	base := "2026-07-14"
	insertDWD(t, db, dwdRow{
		EventID: "ev-me", OrgID: "org1", SeatID: "seat1",
		ProviderCode: "anthropic", ProtocolType: "anthropic",
		Model: "claude", EventTimeMs: msAt("2026-07-14T10:00:00Z"),
		UsageDate: base, TotalTokens: 1000, RequestCount: 5,
	})
	insertDWD(t, db, dwdRow{
		EventID: "ev-agent1", OrgID: "org1", SeatID: "agent1",
		ProviderCode: "anthropic", ProtocolType: "anthropic",
		Model: "claude", EventTimeMs: msAt("2026-07-14T10:05:00Z"),
		UsageDate: base, TotalTokens: 300, RequestCount: 2,
	})
	// Stranger's agent burns tokens too — but under a different owner.
	insertDWD(t, db, dwdRow{
		EventID: "ev-agent2", OrgID: "org1", SeatID: "agent2",
		ProviderCode: "anthropic", ProtocolType: "anthropic",
		Model: "claude", EventTimeMs: msAt("2026-07-14T10:07:00Z"),
		UsageDate: base, TotalTokens: 9999, RequestCount: 40,
	})

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-07-01")
	end, _ := time.Parse("2006-01-02", "2026-07-31")

	rows, err := repo.PersonalByAgentTotal(context.Background(), QueryParams{
		OrgID: "org1", SeatID: "seat1", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]AgentTotal{}
	for _, r := range rows {
		byID[r.SeatID] = r
	}

	// Negative (the point of the test): stranger's agent must be absent.
	if _, leaked := byID["agent2"]; leaked {
		t.Fatalf("SECURITY: stranger's agent (agent2, parent=seat-other) leaked into caller's by-agent breakdown: %+v", rows)
	}
	if len(rows) != 2 {
		t.Fatalf("want exactly 2 rows (own seat + own agent), got %d: %+v", len(rows), rows)
	}

	// Own human seat row.
	me, ok := byID["seat1"]
	if !ok {
		t.Fatal("caller's own seat row missing")
	}
	if me.IsAgent {
		t.Errorf("own human seat marked IsAgent: %+v", me)
	}
	if me.SeatAlias != "Me" || me.TotalTokens != 1000 || me.RequestCount != 5 {
		t.Errorf("own seat row wrong: %+v", me)
	}

	// Owned agent row.
	a1, ok := byID["agent1"]
	if !ok {
		t.Fatal("caller's agent (agent1) row missing")
	}
	if !a1.IsAgent {
		t.Errorf("agent1 not marked IsAgent: %+v", a1)
	}
	if a1.ParentSeatID != "seat1" {
		t.Errorf("agent1 parent_seat_id = %q, want seat1: %+v", a1.ParentSeatID, a1)
	}
	if a1.SeatAlias != "Bot One" || a1.TotalTokens != 300 || a1.RequestCount != 2 {
		t.Errorf("agent1 row wrong: %+v", a1)
	}
	if a1.LastAccountID != "" || a1.LastRequestAtMs != 0 {
		t.Errorf("latest route must stay opt-in for established callers: %+v", a1)
	}

	// Latest actual-account enrichment uses ODS and keeps the SAME parent-seat
	// authorization boundary. A newer canary must not replace the real request;
	// a stranger Agent in the same org must never leak.
	insertAgentODS := func(eventID, seatID, accountID, identity, routeSource string, eventTime int64) {
		t.Helper()
		if _, err := db.DB.Exec(`
			INSERT INTO usage_event_ods
				(event_id, event_time, occurred_at, org_id, account_id, seat_id,
				 oauth_identity, route_source, request_status, raw_event_json)
			VALUES (?, ?, ?, 'org1', ?, ?, ?, ?, 'success', '{}')`,
			eventID, eventTime, eventTime, accountID, seatID, identity, routeSource); err != nil {
			t.Fatalf("seed agent ODS %s: %v", eventID, err)
		}
	}
	insertAgentODS("ods-agent-real", "agent1", "pool-B", "b@mock.test", "oauth_group", msAt("2026-07-20T10:00:00Z"))
	insertAgentODS("ods-agent-canary", "agent1", "pool-Z", "z@mock.test", "canary", msAt("2026-07-20T11:00:00Z"))
	insertAgentODS("ods-stranger", "agent2", "pool-secret", "secret@mock.test", "oauth_group", msAt("2026-07-20T12:00:00Z"))

	enriched, err := repo.PersonalByAgentTotal(context.Background(), QueryParams{
		OrgID: "org1", SeatID: "seat1", StartDate: start, EndDate: end, IncludeLastRoute: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	byID = map[string]AgentTotal{}
	for _, row := range enriched {
		byID[row.SeatID] = row
	}
	if _, leaked := byID["agent2"]; leaked {
		t.Fatalf("SECURITY: stranger latest route leaked: %+v", enriched)
	}
	a1 = byID["agent1"]
	if a1.LastAccountID != "pool-B" || a1.LastOAuthIdentity != "b@mock.test" || a1.LastRequestStatus != "success" {
		t.Fatalf("latest real route = %+v, want pool-B (newer canary excluded)", a1)
	}
	if a1.LastRequestAtMs != msAt("2026-07-20T10:00:00Z") {
		t.Fatalf("last_request_at_ms = %d", a1.LastRequestAtMs)
	}
}

// A personal/BYOK caller has no seat → no agents → empty (not an error, not a
// leak of every seat via an unscoped query).
func TestPersonalByAgentTotal_NoSeatEmpty(t *testing.T) {
	db := setupUsageTestDB(t)
	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", "2026-07-01")
	end, _ := time.Parse("2006-01-02", "2026-07-31")
	rows, err := repo.PersonalByAgentTotal(context.Background(), QueryParams{
		OrgID: "personal", AccountID: "acc-1", StartDate: start, EndDate: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("no-seat caller must get empty by-agent, got %d rows: %+v", len(rows), rows)
	}
}
