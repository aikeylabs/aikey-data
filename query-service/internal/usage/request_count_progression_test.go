package usage

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// N openai-protocol requests must be counted as N — checked after EVERY one.
//
// 🔴 WHY THIS SHAPE (2026-08-24, from a live report). A user sent several codex
// (openai_compatible) requests and the usage ledger kept showing ONE. Every test
// we had sent a single request and asserted a single row, so nothing covered the
// step from 1 to 2. That is exactly where a dedup rule, a stale cache or a bad
// GROUP BY goes wrong: the first request always looks right.
//
// So this asserts after each send, not once at the end. A failure names the
// transition that broke ("2nd request did not raise the count"), which is the
// difference between a bug report and a bug diagnosis.
//
// 🔴 It also reads the count through TWO paths on the same data. On the machine
// that reported this, `aikey status --usage` said 3 requests while the ledger
// page said 1 — same database, two numbers. A test that queries one path can be
// green while the surface the user actually looks at is wrong.
func TestRequestCount_RisesWithEveryRequest(t *testing.T) {
	db := setupUsageTestDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()

	const date = "2026-08-24"
	day, _ := time.Parse("2006-01-02", date)
	base, _ := time.Parse("2006-01-02T15:04:05Z", date+"T10:00:00Z")
	p := QueryParams{SeatID: "seat-openai", StartDate: day, EndDate: day}

	for i := 1; i <= 3; i++ {
		insertDWD(t, db, dwdRow{
			// Distinct request_id per request: this is what the proxy produces
			// (RequestID comes from the per-request TraceContext). Retries — the
			// only case that should collapse — are covered by the test below.
			EventID:      fmt.Sprintf("evt-openai-%d", i),
			RequestID:    fmt.Sprintf("req-openai-%d", i),
			OrgID:        "personal",
			SeatID:       "seat-openai",
			AccountID:    "acct-openai",
			VirtualKeyID: "vk-openai",
			ProviderCode: "codex",
			ProtocolType: "openai_compatible",
			EventTimeMs:  base.Add(time.Duration(i) * time.Minute).UnixMilli(),
			UsageDate:    date,
			TotalTokens:  100,
			RequestCount: 1,
			// stats queries only count normal traffic (scopeStatsAnd)
			UserUsageScope: "normal",
		})

		byProto, err := repo.PersonalByProtocolTotal(ctx, p)
		if err != nil {
			t.Fatalf("after request %d: PersonalByProtocolTotal: %v", i, err)
		}
		var protoReqs int64
		for _, row := range byProto {
			// 🔴 ProtocolTotal.ProtocolType carries provider_code, not the
			// protocol — the struct says so in a trailing comment that is easy
			// to miss. Matching on "openai_compatible" here silently matches
			// nothing and every assertion reads 0.
			if row.ProtocolType == "codex" {
				protoReqs = row.RequestCount
			}
		}
		if protoReqs != int64(i) {
			t.Fatalf("request #%d did not raise the count: by-protocol reports %d requests, want %d.\n"+
				"The step from %d to %d is where a dedup rule or a bad GROUP BY collapses "+
				"distinct requests into one.", i, protoReqs, i, i-1, i)
		}

		// Same data, the other read path. These must never disagree.
		hourly, err := repo.PersonalHourlyTimeline(ctx, p)
		if err != nil {
			t.Fatalf("after request %d: PersonalHourlyTimeline: %v", i, err)
		}
		var hourlyReqs int64
		for _, h := range hourly {
			hourlyReqs += h.RequestCount
		}
		if hourlyReqs != protoReqs {
			t.Fatalf("after request %d the two read paths disagree: hourly says %d, by-protocol says %d.\n"+
				"One surface showing a different number from another on the SAME rows is what a user "+
				"reports as \"the page is wrong\"; it is a query defect, not a data defect.",
				i, hourlyReqs, protoReqs)
		}
	}
}

// A retry of the SAME request must still count once.
//
// 🔴 The other half of the contract, and it must live next to the first: a rule
// that collapses retries can also collapse distinct requests, and a rule that
// counts every row can also count retries twice. Testing one direction leaves
// the other unconstrained — when the dedup was rewritten this session there was
// no test that could have said whether it now over- or under-counted.
func TestRequestCount_RetryOfTheSameRequestStillCountsOnce(t *testing.T) {
	db := setupUsageTestDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()

	const date = "2026-08-24"
	day, _ := time.Parse("2006-01-02", date)
	base, _ := time.Parse("2006-01-02T15:04:05Z", date+"T10:00:00Z")

	// One request, two rows: a failure followed by the successful retry.
	for i, st := range []struct {
		status string
		offset time.Duration
	}{{"failed", 0}, {"success", time.Second}} {
		insertDWD(t, db, dwdRow{
			EventID:        fmt.Sprintf("evt-retry-%d", i),
			RequestID:      "req-retried-once",
			OrgID:          "personal",
			SeatID:         "seat-retry",
			AccountID:      "acct-openai",
			VirtualKeyID:   "vk-openai",
			ProviderCode:   "codex",
			ProtocolType:   "openai_compatible",
			EventTimeMs:    base.Add(st.offset).UnixMilli(),
			UsageDate:      date,
			TotalTokens:    100,
			RequestCount:   1,
			RequestStatus:  st.status,
			UserUsageScope: "normal",
		})
	}

	rows, err := repo.PersonalByProtocolTotal(ctx,
		QueryParams{SeatID: "seat-retry", StartDate: day, EndDate: day})
	if err != nil {
		t.Fatalf("PersonalByProtocolTotal: %v", err)
	}
	var reqs int64
	for _, r := range rows {
		if r.ProtocolType == "codex" {
			reqs = r.RequestCount
		}
	}
	if reqs != 1 {
		t.Fatalf("one request retried once counted as %d, want 1 — "+
			"every upstream retry would inflate the user's request count", reqs)
	}
}
