package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/conversation"
)

// spyConvRepo records the QueryParams the handler builds so the WIRE PARSING of
// ?seat_keys= is fenced independently of the SQL (which repository_sql_test.go
// covers). This is the query-service half of the facade↔query-service seat
// search contract; the facade half (control-master usage_facade_test.go)
// asserts what gets SENT, this asserts how it is READ — together they pin the
// contract without an integration environment.
type spyConvRepo struct {
	fakeConvRepo
	got conversation.QueryParams
}

func (s *spyConvRepo) SeatSummaries(_ context.Context, p conversation.QueryParams) ([]conversation.SeatSummary, int64, error) {
	s.got = p
	return []conversation.SeatSummary{}, 0, nil
}

func TestSeatsHandlerParsesSeatSearchParams(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		wantSet  bool
		wantKeys []string
		wantLike string
	}{
		// Neither param = no search at all (the pre-search behavior).
		{"absent", "org_id=o1", false, nil, ""},
		// Both present but empty = the facade's "matched nothing" — MUST stay
		// distinguishable from absent, or a failed search shows everyone.
		{"present empty", "org_id=o1&seat_keys=&seat_key_like=", true, nil, ""},
		{"keys only", "org_id=o1&seat_keys=st-1,acc-1", true, []string{"st-1", "acc-1"}, ""},
		// The directory-less case: no key resolved, raw term carries the search.
		{"like only", "org_id=o1&seat_keys=&seat_key_like=demo-alice", true, nil, "demo-alice"},
		{"both", "org_id=o1&seat_keys=st-1&seat_key_like=alice", true, []string{"st-1"}, "alice"},
		// Blank segments (",,") and padding are dropped, not turned into "" keys.
		{"blank segments", "org_id=o1&seat_keys=st-1,,%20st-2%20", true, []string{"st-1", "st-2"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &spyConvRepo{}
			w := httptest.NewRecorder()
			NewConversationHandler(repo).Seats(w, httptest.NewRequest("GET", "/v1/conversation-audit/seats?"+tc.query, nil))
			if w.Code != 200 {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			if repo.got.SeatFilterSet != tc.wantSet {
				t.Errorf("SeatFilterSet = %v, want %v", repo.got.SeatFilterSet, tc.wantSet)
			}
			if got, want := strings.Join(repo.got.SeatKeys, ","), strings.Join(tc.wantKeys, ","); got != want {
				t.Errorf("SeatKeys = %q, want %q", got, want)
			}
			if repo.got.SeatKeyLike != tc.wantLike {
				t.Errorf("SeatKeyLike = %q, want %q", repo.got.SeatKeyLike, tc.wantLike)
			}
		})
	}
}

func TestSeatsHandlerRejectsOversizedSeatKeys(t *testing.T) {
	repo := &spyConvRepo{}
	keys := make([]string, maxSeatKeys+1)
	for i := range keys {
		keys[i] = "k"
	}
	w := httptest.NewRecorder()
	NewConversationHandler(repo).Seats(w,
		httptest.NewRequest("GET", "/v1/conversation-audit/seats?org_id=o1&seat_keys="+strings.Join(keys, ","), nil))
	if w.Code != 400 {
		t.Fatalf("status = %d, want explicit 400 (no silent truncation): %s", w.Code, w.Body.String())
	}
}
