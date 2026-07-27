package usage

import (
	"os"
	"regexp"
	"testing"
)

func TestAggregateQueriesNeverSumUpstreamAttemptRequestCount(t *testing.T) {
	src, err := os.ReadFile("repository_sql.go")
	if err != nil {
		t.Fatalf("read repository_sql.go: %v", err)
	}
	attemptSum := regexp.MustCompile(`(?i)SUM\(\s*(?:d\.)?request_count\s*\)`)
	if loc := attemptSum.FindIndex(src); loc != nil {
		t.Fatalf("aggregate query sums per-attempt request_count at byte %d; use usage_reporting_fact.client_request_count", loc[0])
	}
}
