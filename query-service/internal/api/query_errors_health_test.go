package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A query failure must be readable from /health, not only from the log.
//
// 🔴 WHY (2026-09-22): MasterUpstreamLatency returned 500 on every call for two
// months; the only evidence was a log line nobody was alerting on. A signal an
// operator cannot poll is not a signal (principles/health-signal-surface.md).
// bugfix: workflow/CI/bugfix/2026-09-22-query-service-upstream-latency-wrong-table.md
func TestHealthExposesQueryErrorCounters(t *testing.T) {
	queryErrors.reset()
	t.Cleanup(queryErrors.reset)
	router := NewRouter(nil, nil, nil, nil, "tok")

	health := func() map[string]any {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("/health must stay 200 (installers gate on it), got %d", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	before := health()
	if before["status"] != "ok" {
		t.Fatalf("status must be ok, got %v", before["status"])
	}
	if qe := before["query_errors"].(map[string]any); qe["total"].(float64) != 0 {
		t.Fatalf("fresh process must report 0 query errors, got %v", qe)
	}

	rec := httptest.NewRecorder()
	respondQueryError(rec, "MasterUpstreamLatency", errors.New(`pq: relation "usage_fact_ods" does not exist`))
	rec = httptest.NewRecorder()
	respondQueryError(rec, "MasterUpstreamLatency", errors.New("query: context deadline exceeded"))

	after := health()
	qe := after["query_errors"].(map[string]any)
	if qe["total"].(float64) != 2 {
		t.Fatalf("want total=2 after one failure and one timeout, got %v", qe)
	}
	byCode := qe["by_code"].(map[string]any)
	if byCode["QUERY_FAILED"].(float64) != 1 || byCode["QUERY_TIMEOUT"].(float64) != 1 {
		t.Fatalf("want by_code {QUERY_FAILED:1, QUERY_TIMEOUT:1}, got %v", byCode)
	}
	if qe["by_op"].(map[string]any)["MasterUpstreamLatency"].(float64) != 2 {
		t.Fatalf("want by_op MasterUpstreamLatency=2, got %v", qe["by_op"])
	}
	last := qe["last"].(map[string]any)
	if last["code"] != "QUERY_TIMEOUT" || last["op"] != "MasterUpstreamLatency" || last["at"] == "" {
		t.Fatalf("last must name op/code/at of the most recent error, got %v", last)
	}
	if after["status"] != "ok" {
		t.Fatalf("status must remain ok after query errors, got %v", after["status"])
	}
}
