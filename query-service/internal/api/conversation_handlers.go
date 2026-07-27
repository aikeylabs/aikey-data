package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/conversation"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

// ConversationHandler serves the enterprise conversation-audit read views
// (seat list → session list → thread drawer). The master console facades
// /v1/conversation-audit/* to these (mirroring the usage facade); org_id is
// passed as a query param by the facade.
type ConversationHandler struct {
	repo conversation.Repository
}

func NewConversationHandler(repo conversation.Repository) *ConversationHandler {
	return &ConversationHandler{repo: repo}
}

// listResponse is the paginated wire envelope for the seat / session lists.
//
// WHY it exists (2026-07-26): these two endpoints used to return a BARE JSON
// ARRAY. Without a total the console cannot know how many pages exist, so it
// could only render a blind prev/next — and it guessed "there may be more" from
// `len(page) >= pageSize`, which offers a dead "next" whenever the row count is
// an exact multiple of the page size. Shipping the total lets the console render
// real page numbers and disable next exactly when it should.
//
// Shape deliberately mirrors the already-shipped compliance audit-log envelope
// (`{events,total,limit,offset}` in aikey-control-master compliance/handler_audit.go)
// so one Pagination component drives both. `items` rather than `seats`/`sessions`
// because two list views share this type; compliance's `events` key stays frozen.
//
// ⚠️ The console reads BOTH shapes (array | envelope) on purpose: query-service
// and control-master are independent systemd units in the Cluster/Production
// editions, so a deploy window can pair a new SPA with an old service or vice
// versa. Do not "simplify" the client back to a single shape.
type listResponse[T any] struct {
	Items  []T   `json:"items"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

// Seats handles GET /v1/conversation-audit/seats
//
//	?org_id=&start_date=&end_date=&limit=&offset=
func (h *ConversationHandler) Seats(w http.ResponseWriter, r *http.Request) {
	p, err := parseConvParams(r, false, false)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, total, err := h.repo.SeatSummaries(r.Context(), p)
	if err != nil {
		slog.Error("conversation seats query failed", "event.name", "conversation.query.seats_failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []conversation.SeatSummary{}
	}
	shared.JSON(w, http.StatusOK, listResponse[conversation.SeatSummary]{
		Items: data, Total: total,
		Limit: conversation.EffectiveListLimit(p.Limit), Offset: p.Offset,
	})
}

// Sessions handles GET /v1/conversation-audit/sessions
//
//	?org_id=&owner_account_id=&start_date=&end_date=&limit=&offset=
func (h *ConversationHandler) Sessions(w http.ResponseWriter, r *http.Request) {
	p, err := parseConvParams(r, true, false)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, total, err := h.repo.SessionSummaries(r.Context(), p)
	if err != nil {
		slog.Error("conversation sessions query failed", "event.name", "conversation.query.sessions_failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []conversation.SessionSummary{}
	}
	shared.JSON(w, http.StatusOK, listResponse[conversation.SessionSummary]{
		Items: data, Total: total,
		Limit: conversation.EffectiveListLimit(p.Limit), Offset: p.Offset,
	})
}

// Thread handles GET /v1/conversation-audit/thread
//
//	?org_id=&owner_account_id=&session_id=&limit=&offset=
func (h *ConversationHandler) Thread(w http.ResponseWriter, r *http.Request) {
	p, err := parseConvParams(r, true, true)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.ThreadDetail(r.Context(), p)
	if err != nil {
		slog.Error("conversation thread query failed", "event.name", "conversation.query.thread_failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	shared.JSON(w, http.StatusOK, data)
}

// parseConvParams extracts the tenant-scoped query params. org_id is always
// required; owner_account_id / session_id are required for the narrower views
// (r2 #4 — session_id is read only within org + owner). A malformed date param
// is ignored rather than passed into the conv_date filter.
func parseConvParams(r *http.Request, requireOwner, requireSession bool) (conversation.QueryParams, error) {
	q := r.URL.Query()
	orgID := q.Get("org_id")
	if orgID == "" {
		return conversation.QueryParams{}, errMissing("org_id")
	}
	p := conversation.QueryParams{
		OrgID:          orgID,
		OwnerAccountID: q.Get("owner_account_id"),
		SessionID:      q.Get("session_id"),
		StartDate:      validConvDate(q.Get("start_date")),
		EndDate:        validConvDate(q.Get("end_date")),
		// Sort key/dir for the clickable list headers; the repo whitelists the
		// key (unknown → list default), so no validation needed here.
		SortBy:  q.Get("sort"),
		SortDir: q.Get("dir"),
	}
	if requireOwner && p.OwnerAccountID == "" {
		return p, errMissing("owner_account_id")
	}
	// session_id "" is VALID — the pseudo-session grouping conversations captured
	// without a session identifier (NULL session_id, COALESCE'd to "" in the
	// queries). Require the PARAM to be PRESENT (q.Has), not the value non-empty,
	// else the "(no session)" thread drawer 400s ("session_id is required") even
	// though the turns exist. The seat→session list already surfaces the "" group;
	// drilling into it must not be rejected. Bugfix:
	// 2026-06-17-conversation-audit-query-null-session-id.md
	if requireSession && !q.Has("session_id") {
		return p, errMissing("session_id")
	}
	if lim := q.Get("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil && n > 0 {
			p.Limit = n
		}
	}
	if off := q.Get("offset"); off != "" {
		if n, err := strconv.Atoi(off); err == nil && n >= 0 {
			p.Offset = n
		}
	}
	return p, nil
}

func validConvDate(s string) string {
	if s == "" {
		return ""
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return ""
	}
	return s
}
