package usage

import (
	"context"
	"testing"
	"time"
)

// TestMasterUsageDetail_CarriesFallbackAttemptAndReason pins that the per-event
// AUDIT drill-down exposes which hop served a request, and why it got there.
//
// 🔴 Why this is a defect and not a feature request. usage_fact_dwd has carried
// fallback_attempt / fallback_reason since P0a shipped, and the AGGREGATE view
// (MasterUpstreamStepArounds) reads them — so the console can already tell an
// admin "12 step-arounds happened this week". The drill-down they open next,
// to see WHICH request failed over, silently dropped both columns. The roll-up
// and the detail therefore disagreed about what the warehouse knows, and the
// admin's only per-request answer was "no information here", for data that was
// sitting one SELECT away. Found on staging 2026-08-04 against real rows
// (fallback_attempt=2, reason=UPSTREAM_SERVER_ERROR).
//
// 能红: drop `d.fallback_attempt, COALESCE(d.fallback_reason,'')` from
// masterAuditSelect (and its two scan targets) → the switched row reports no
// hop and the three states below collapse into one.
//
// The three states are the whole point, so all three are asserted:
//
//	NULL → single-shot: this key has no chain, or the row predates the field.
//	   1 → the PRIMARY served it. Healthy, and by far the most common row.
//	  2+ → a fallback served it, and fallback_reason says what went wrong.
//
// 🚫 NULL must never arrive as 0 or 1. "No failover is configured for this key"
// and "failover is configured and the primary was fine" are different answers
// to the question the page exists to answer, and COALESCE would erase the
// difference in exactly the direction that looks reassuring.
func TestMasterUsageDetail_CarriesFallbackAttemptAndReason(t *testing.T) {
	db := setupUsageTestDB(t)
	seedOrgSeatsFixture(t, db) // masterAuditSelect LEFT JOINs org_seats for the alias
	day, _ := time.Parse("2006-01-02", "2026-07-20")
	base, _ := time.Parse(time.RFC3339, "2026-07-20T10:00:00Z")
	baseMs := base.UnixMilli()

	seed := func(id, provider string, attempt, reason any, offsetMin int64) {
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

	seed("e-single", "anthropic", nil, nil, 0)
	seed("e-primary", "anthropic", 1, nil, 1)
	seed("e-switched", "zhipu", 2, "UPSTREAM_SERVER_ERROR", 2)

	rows, err := NewSQLRepository(db).MasterUsageDetail(context.Background(), QueryParams{
		OrgID: "org1", StartDate: day, EndDate: day, Limit: 50, Offset: 0,
	})
	if err != nil {
		t.Fatalf("MasterUsageDetail: %v", err)
	}
	byID := map[string]MasterUsageAuditRow{}
	for _, r := range rows {
		byID[r.EventID] = r
	}
	if len(byID) != 3 {
		t.Fatalf("seeded 3 events, detail returned %d: %v", len(byID), byID)
	}

	if got := byID["e-single"]; got.FallbackAttempt != nil {
		t.Errorf("single-shot row reported hop %d; want null — a key with no chain "+
			"has no hop number, and calling it hop 1 claims a primary/fallback split "+
			"that does not exist", *got.FallbackAttempt)
	}

	primary := byID["e-primary"]
	if primary.FallbackAttempt == nil || *primary.FallbackAttempt != 1 {
		t.Errorf("primary-served row FallbackAttempt = %v, want 1 — the drill-down has "+
			"to distinguish 'the primary served it' from 'there is no chain'",
			fmtAttempt(primary.FallbackAttempt))
	}
	if primary.FallbackReason != "" {
		t.Errorf("primary-served row carries reason %q; nothing went wrong on it", primary.FallbackReason)
	}

	switched := byID["e-switched"]
	if switched.FallbackAttempt == nil || *switched.FallbackAttempt != 2 {
		t.Errorf("switched row FallbackAttempt = %v, want 2 — without this the audit "+
			"page cannot show that this request failed over",
			fmtAttempt(switched.FallbackAttempt))
	}
	if switched.FallbackReason != "UPSTREAM_SERVER_ERROR" {
		t.Errorf("switched row FallbackReason = %q, want UPSTREAM_SERVER_ERROR — the "+
			"hop number alone does not say why the primary was abandoned", switched.FallbackReason)
	}
}

func fmtAttempt(p *int64) any {
	if p == nil {
		return "null"
	}
	return *p
}
