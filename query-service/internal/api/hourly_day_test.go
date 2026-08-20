package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

// The desktop app's usage headline sat at 0 for every user because
// GET .../usage/personal/hourly WITHOUT `date` answered for the single day 30
// days in the past, not today: parsePersonalParams → Defaults() fills
// StartDate with `EndDate-30d` (the multi-day default, meaningless here), so
// the handler's `if p.StartDate.IsZero()` "today" fallback was dead code.
// HTTP 200 + `[]` reads on screen as "you used nothing today", so nothing
// upstream ever raised a hand.
//
// This fences the CONTRACT, not the line I changed: whatever the handler does
// internally, an absent `date` must select the same day as `date=<today>`, and
// must never select today-30d. 能红: put the `p.StartDate.IsZero()` guard back
// and case "date omitted" fails with a window 30 days wide of the mark.

// dayCapturingRepo records the window the handler asks for. Embedding mockRepo
// keeps it a full usage.Repository without restating methods this test does
// not care about.
type dayCapturingRepo struct {
	mockRepo
	got usage.QueryParams
}

func (r *dayCapturingRepo) PersonalHourlyTimeline(_ context.Context, p usage.QueryParams) ([]usage.HourlyPoint, error) {
	r.got = p
	return nil, nil
}

func hourlyWindow(t *testing.T, query string) usage.QueryParams {
	t.Helper()
	repo := &dayCapturingRepo{}
	h := NewUsageHandler(repo)
	req := httptest.NewRequest("GET", "/v1/usage/personal/hourly?"+query, nil)
	w := httptest.NewRecorder()
	h.PersonalHourlyTimeline(w, req)
	if w.Code != 200 {
		t.Fatalf("query %q: expected 200, got %d body=%s", query, w.Code, w.Body.String())
	}
	return repo.got
}

func TestPersonalHourly_DaySelection(t *testing.T) {
	// A zone well away from UTC, so a handler that resolved "today" in the
	// wrong location would land on a different calendar day for part of the
	// day rather than passing by luck.
	const tz = "Asia/Shanghai"
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load %s: %v", tz, err)
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	cases := []struct {
		name  string
		query string
		want  time.Time
	}{
		{"date omitted", "org_id=personal&tz=" + tz, today},
		{"date=today", "org_id=personal&tz=" + tz + "&date=" + today.Format("2006-01-02"), today},
		{"date unparseable", "org_id=personal&tz=" + tz + "&date=not-a-date", today},
		{"explicit past date is honoured", "org_id=personal&tz=" + tz + "&date=2026-04-24",
			time.Date(2026, 4, 24, 0, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hourlyWindow(t, c.query)
			if !got.StartDate.Equal(c.want) {
				t.Errorf("start day = %s, want %s", got.StartDate, c.want)
			}
			// The endpoint answers for ONE day; a widened window would let
			// yesterday's traffic leak into today's hour buckets.
			if !got.EndDate.Equal(got.StartDate) {
				t.Errorf("window is not a single day: start=%s end=%s", got.StartDate, got.EndDate)
			}
		})
	}
}

// Named separately from the table above because this exact day is the
// regression, and a failure should say so rather than read as an off-by-one.
func TestPersonalHourly_OmittedDateIsNotThirtyDaysAgo(t *testing.T) {
	const tz = "Asia/Shanghai"
	loc, _ := time.LoadLocation(tz)
	got := hourlyWindow(t, "org_id=personal&tz="+tz)

	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	stale := today.AddDate(0, 0, -30) // QueryParams.Defaults()'s multi-day StartDate

	if got.StartDate.Equal(stale) {
		t.Fatalf("omitting ?date selected %s — the Defaults() 30-day range start leaked into "+
			"this single-day endpoint again; every caller that omits `date` now reads 0",
			got.StartDate.Format("2006-01-02"))
	}
}
