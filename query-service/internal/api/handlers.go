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
	if seatID == "" && accountID == "" {
		return usage.QueryParams{}, errMissing("seat_id or account_id")
	}
	p := usage.QueryParams{SeatID: seatID, AccountID: accountID}
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
	p := usage.QueryParams{OrgID: orgID}
	parseDates(&p, q.Get("start_date"), q.Get("end_date"))
	if lim := q.Get("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil && n > 0 {
			p.Limit = n
		}
	}
	p.Defaults()
	return p, nil
}

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
