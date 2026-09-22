package usage

import (
	"context"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

// MasterUpstreamLatency must run against the real ODS schema.
//
// 🔴 WHY (2026-09-22). The query shipped on 2026-07-30 (task 5.7) reading
// `FROM usage_fact_ods` — a table that has never existed (the ODS is
// `usage_event_ods`). Every open of the console's threshold page returned
// 500 QUERY_FAILED for two months and nobody saw it: the handler test used a
// mock repository, the SQL was never executed by any test, the backend only
// logged, and the console is designed to drop the warning when the figure is
// missing. This test executes the real SQL on the real baseline DDL, so a wrong
// table or column name is red here instead of silent in production.
// bugfix: workflow/CI/bugfix/2026-09-22-query-service-upstream-latency-wrong-table.md
// spec: R-upstream-fallback-11 上游 P95 延迟分布的语义
func seedLatencyRow(t *testing.T, db *shared.DB, eventID string, eventTimeMs, startedMs, finishedMs int64) {
	t.Helper()
	_, err := db.DB.Exec(`
		INSERT INTO usage_event_ods (
			event_id, event_time, occurred_at, org_id,
			started_at, finished_at,
			request_status, raw_event_json
		) VALUES (?, ?, ?, 'org-1', ?, ?, 'success', '{}')`,
		eventID, eventTimeMs, eventTimeMs, startedMs, finishedMs)
	if err != nil {
		t.Fatalf("seed latency row %q: %v", eventID, err)
	}
}

func latencyParams() QueryParams {
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	return QueryParams{OrgID: "org-1", StartDate: day, EndDate: day}
}

// R-upstream-fallback-11.S1: zero samples → Samples=0 and P95Ms=0 together,
// no error. "P95 = 0" alone would read as "instant"; the console must see the
// sample count to know it is "unknown".
func TestMasterUpstreamLatency_NoSamplesIsNotAnError(t *testing.T) {
	repo := NewSQLRepository(setupUsageTestDB(t))
	got, err := repo.MasterUpstreamLatency(context.Background(), latencyParams())
	if err != nil {
		t.Fatalf("query against real schema failed: %v", err)
	}
	if got.Samples != 0 || got.P95Ms != 0 || got.WindowDays != 7 {
		t.Fatalf("empty org: want Samples=0 P95Ms=0 WindowDays=7, got %+v", got)
	}
}

// R-upstream-fallback-11.S2: P95 is the ⌊n·0.95⌋-th smallest latency of the
// org's rows inside the window; rows without started/finished, outside the
// window, or of another org do not count. Expected values are hand-derived
// literals, not computed by the code under test.
func TestMasterUpstreamLatency_P95FromRealRows(t *testing.T) {
	db := setupUsageTestDB(t)
	repo := NewSQLRepository(db)
	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC).UnixMilli()
	// 20 rows with latencies 100, 200, …, 2000 ms → idx = 20*95/100 = 19 → 2000.
	for i := 1; i <= 20; i++ {
		seedLatencyRow(t, db, "ev-"+string(rune('a'+i-1)), base+int64(i), base, base+int64(i*100))
	}
	// Noise that must not count: no finished_at; outside the window; other org.
	if _, err := db.DB.Exec(`INSERT INTO usage_event_ods (event_id, event_time, occurred_at, org_id, started_at, request_status, raw_event_json)
		VALUES ('ev-open', ?, ?, 'org-1', ?, 'success', '{}')`, base, base, base); err != nil {
		t.Fatal(err)
	}
	seedLatencyRow(t, db, "ev-yesterday", base-48*3600*1000, base, base+99_000)
	if _, err := db.DB.Exec(`INSERT INTO usage_event_ods (event_id, event_time, occurred_at, org_id, started_at, finished_at, request_status, raw_event_json)
		VALUES ('ev-other-org', ?, ?, 'org-2', ?, ?, 'success', '{}')`, base, base, base, base+99_000); err != nil {
		t.Fatal(err)
	}

	got, err := repo.MasterUpstreamLatency(context.Background(), latencyParams())
	if err != nil {
		t.Fatalf("query against real schema failed: %v", err)
	}
	if got.Samples != 20 || got.P95Ms != 2000 {
		t.Fatalf("want Samples=20 P95Ms=2000, got %+v", got)
	}
}
