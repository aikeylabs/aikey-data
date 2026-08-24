package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
)

// A cancelled/timed-out query must be reported as a TIMEOUT, not as a generic
// internal error.
//
// 🔴 WHY (2026-08-24). In the field five aggregate endpoints ran past the
// caller's 15s deadline; Postgres reported `pq: canceling statement due to user
// request (57014)` and every one of them came back as
// `500 QUERY_FAILED "internal error"`. The team-usage page went blank ~90% of
// the time and neither the user nor the operator was told why — the log line
// carried no event.name and no error.code, so there was nothing to alert on
// either. Slow must be distinguishable from broken.
func TestQueryTimeoutIsReportedAsTimeoutNotInternalError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"context deadline", context.DeadlineExceeded},
		{"context cancelled", context.Canceled},
		{"postgres statement cancel", errors.New("personal by-app total: pq: canceling statement due to user request (57014)")},
		{"wrapped deadline text", errors.New("query: context deadline exceeded")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			respondQueryError(rec, "PersonalByAppTotal", tc.err)
			if rec.Code != http.StatusGatewayTimeout {
				t.Fatalf("status = %d, want 504 — a timeout reported as 500 sends the operator hunting a database fault that is not there", rec.Code)
			}
			if body := rec.Body.String(); !regexp.MustCompile(`QUERY_TIMEOUT`).MatchString(body) {
				t.Fatalf("body has no QUERY_TIMEOUT code, so the UI cannot tell the user anything useful: %s", body)
			}
		})
	}
}

// A genuine failure must still be a 500 — the timeout branch must not swallow
// real faults and make a broken database look merely slow.
func TestGenuineQueryFailureStaysInternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	respondQueryError(rec, "PersonalByAppTotal", errors.New("pq: relation \"usage_fact_dwd\" does not exist"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a missing table is not a timeout", rec.Code)
	}
	if body := rec.Body.String(); !regexp.MustCompile(`QUERY_FAILED`).MatchString(body) {
		t.Fatalf("body lost the QUERY_FAILED code: %s", body)
	}
}

// Every handler must route its repository error through the single exit.
//
// 🔴 A SOURCE fence, and deliberately so. Twenty handlers share this shape; a
// twenty-first written by hand would silently reintroduce the flattening this
// whole change removed, and no behaviour test would notice because that one
// endpoint simply would not be covered.
func TestEveryHandlerUsesTheSharedQueryErrorExit(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	// The old inline shape must not come back anywhere.
	inline := regexp.MustCompile(`shared\.Error\(w, http\.StatusInternalServerError, "QUERY_FAILED"`)
	if hits := inline.FindAllIndex(src, -1); len(hits) > 1 {
		t.Fatalf("%d handlers still answer QUERY_FAILED inline; they must call respondQueryError so a timeout is not reported as an internal error", len(hits)-1)
	}
	if n := len(regexp.MustCompile(`respondQueryError\(w, "`).FindAll(src, -1)); n < 20 {
		t.Fatalf("only %d handlers route through respondQueryError, expected at least 20 — a handler was added without the shared exit", n)
	}
}
