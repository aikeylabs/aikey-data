// Package api provides HTTP handlers for the query service.
package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

// UsageHandler handles usage query endpoints.
type UsageHandler struct {
	repo usage.Repository
}

func NewUsageHandler(repo usage.Repository) *UsageHandler {
	return &UsageHandler{repo: repo}
}

// --- Personal page ---

// GET /v1/usage/personal/timeline?seat_id=...&start_date=...&end_date=...
func (h *UsageHandler) PersonalTimeline(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.PersonalTimeline(r.Context(), p)
	if err != nil {
		slog.Error("PersonalTimeline query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.TimelinePoint{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// GET /v1/usage/personal/hourly?seat_id=...&date=YYYY-MM-DD&tz=<IANA>
// Returns intra-day usage buckets for a single calendar date. As of
// v1.0.3-alpha / bugfix 20260424, buckets are in the **caller's local
// timezone** (per `?tz=`, defaults to UTC): `date` is interpreted as
// the calendar day in that zone, and `hour` is 0..23 local. A +08:00
// caller asking about their local 04-24 thus gets 24 hour-buckets
// from local midnight through the next, with `hour=12` being local
// noon (what was UTC 04:00). Endpoint intentionally ignores end_date
// — the bucket shape only makes sense for a single day.
func (h *UsageHandler) PersonalHourlyTimeline(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	// Override date range: collapse to a single day. If the client
	// passed `date=YYYY-MM-DD`, interpret it in the caller's local tz
	// (matching parsePersonalParams → Defaults() semantics for
	// start_date / end_date). Failing to re-apply toLocalMidnight here
	// would undo the tz shift and produce a UTC-day window — see code
	// review HIGH #1 for the original bug.
	q := r.URL.Query()
	if ds := q.Get("date"); ds != "" {
		if t, err := time.Parse("2006-01-02", ds); err == nil {
			y, m, d := t.Date()
			p.StartDate = time.Date(y, m, d, 0, 0, 0, 0, p.TZLocation)
		}
	}
	if p.StartDate.IsZero() {
		now := time.Now().In(p.TZLocation)
		p.StartDate = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, p.TZLocation)
	}
	p.EndDate = p.StartDate
	data, err := h.repo.PersonalHourlyTimeline(r.Context(), p)
	if err != nil {
		slog.Error("PersonalHourlyTimeline query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.HourlyPoint{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// GET /v1/usage/personal/by-protocol/timeline?seat_id=...&start_date=...&end_date=...
func (h *UsageHandler) PersonalByProtocolTimeline(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.PersonalByProtocolTimeline(r.Context(), p)
	if err != nil {
		slog.Error("PersonalByProtocolTimeline query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.ProtocolTimelinePoint{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// GET /v1/usage/personal/by-protocol/total?seat_id=...&start_date=...&end_date=...
func (h *UsageHandler) PersonalByProtocolTotal(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.PersonalByProtocolTotal(r.Context(), p)
	if err != nil {
		slog.Error("PersonalByProtocolTotal query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.ProtocolTotal{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// GET /v1/usage/personal/by-key/total?seat_id=...&start_date=...&end_date=...
// OR account_id=...
func (h *UsageHandler) PersonalByKeyTotal(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.PersonalByKeyTotal(r.Context(), p)
	if err != nil {
		slog.Error("PersonalByKeyTotal query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.KeyTotal{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// --- Master page ---

// GET /v1/usage/master/ranking?org_id=...&start_date=...&end_date=...&limit=...
func (h *UsageHandler) MasterUserRanking(w http.ResponseWriter, r *http.Request) {
	p, err := parseMasterParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.MasterUserRanking(r.Context(), p)
	if err != nil {
		slog.Error("MasterUserRanking query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.UserRanking{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// GET /v1/usage/master/by-protocol/total?org_id=...&start_date=...&end_date=...
func (h *UsageHandler) MasterByProtocolTotal(w http.ResponseWriter, r *http.Request) {
	p, err := parseMasterParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.MasterByProtocolTotal(r.Context(), p)
	if err != nil {
		slog.Error("MasterByProtocolTotal query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.ProtocolTotal{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// GET /v1/usage/master/timeline?org_id=...&start_date=...&end_date=...
func (h *UsageHandler) MasterTimeline(w http.ResponseWriter, r *http.Request) {
	p, err := parseMasterParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.MasterTimeline(r.Context(), p)
	if err != nil {
		slog.Error("MasterTimeline query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.TimelinePoint{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// --- param parsers ---

func parsePersonalParams(r *http.Request) (usage.QueryParams, error) {
	q := r.URL.Query()
	seatID := q.Get("seat_id")
	accountID := q.Get("account_id")
	orgID := q.Get("org_id")
	// Allow org_id=personal for personal edition (local-user mode) where
	// events have no account_id but are tagged with org_id="personal".
	if seatID == "" && accountID == "" && orgID != "personal" {
		return usage.QueryParams{}, errMissing("seat_id or account_id")
	}
	p := usage.QueryParams{
		SeatID:    seatID,
		AccountID: accountID,
		OrgID:     orgID,
		TZ:        q.Get("tz"), // IANA name; empty → UTC in Defaults()
	}
	parseDates(&p, q.Get("start_date"), q.Get("end_date"))
	p.Defaults()
	return p, nil
}

func parseMasterParams(r *http.Request) (usage.QueryParams, error) {
	q := r.URL.Query()
	orgID := q.Get("org_id")
	if orgID == "" {
		return usage.QueryParams{}, errMissing("org_id")
	}
	p := usage.QueryParams{OrgID: orgID, TZ: q.Get("tz")}
	parseDates(&p, q.Get("start_date"), q.Get("end_date"))
	if lim := q.Get("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil && n > 0 {
			p.Limit = n
		}
	}
	p.Defaults()
	return p, nil
}

// parseDates reads start_date / end_date query parameters as YYYY-MM-DD
// and stores them as time.Time at the respective midnight. The date is
// parsed naive here; repo code shifts to the user's local-tz midnight
// via QueryParams.TZLocation when computing window boundaries.
func parseDates(p *usage.QueryParams, startStr, endStr string) {
	if startStr != "" {
		if t, err := time.Parse("2006-01-02", startStr); err == nil {
			p.StartDate = t
		}
	}
	if endStr != "" {
		if t, err := time.Parse("2006-01-02", endStr); err == nil {
			p.EndDate = t
		}
	}
}

type paramError struct{ field string }

func (e paramError) Error() string { return e.field + " is required" }
func errMissing(f string) error    { return paramError{field: f} }
