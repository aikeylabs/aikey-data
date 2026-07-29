// Package api provides HTTP handlers for the query service.
package api

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

const resolvedOwnerScopeHeader = "X-Aikey-Resolved-Owner-Scope"

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

// GET /v1/usage/personal/by-protocol/hourly?seat_id=...&date=YYYY-MM-DD&tz=<IANA>
// (or account_id / org_id=personal)
//
// Intra-day per-provider stacked-bar source for the "1D" range option
// on /user/usage-ledger. Like /personal/hourly above, parses ?date as
// p.StartDate in the caller's local tz (parsePersonalParams handles
// only ?start_date/?end_date — ?date semantics are hourly-handler
// specific). SQL impl consumes StartDate + TZOffsetMs.
func (h *UsageHandler) PersonalByProtocolHourly(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	// Date param: same shape as PersonalHourlyTimeline above. Default
	// to "today in tz" when omitted so the chart never returns stale
	// data from a default 30-day-ago window.
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
	data, err := h.repo.PersonalByProtocolHourly(r.Context(), p)
	if err != nil {
		slog.Error("PersonalByProtocolHourly query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.ProtocolHourlyPoint{}
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

// GET /v1/usage/personal/by-app/total?seat_id=...&start_date=...&end_date=...
// Returns rows grouped by (app_slug, provider_code) for the 2026-05-25
// /user/usage-ledger "Usage By App" ranking chart. Empty app_slug
// indicates "direct /v1/..." traffic (no Connected App context) — the
// frontend renders those as the CLI tool name derived from
// provider_code (anthropic→claude, openai→codex, kimi_code→kimi, ...).
// See usage.AppTotal docstring for the full shape contract.
func (h *UsageHandler) PersonalByAppTotal(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.PersonalByAppTotal(r.Context(), p)
	if err != nil {
		slog.Error("PersonalByAppTotal query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.AppTotal{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// GET /v1/usage/personal/by-agent/total?seat_id=...&start_date=...&end_date=...
// Returns usage grouped by seat_id for the caller's own seat + their Agent
// seats (org_seats.parent_seat_id = root seat) — the /user/usage-ledger
// "Usage By Agent" breakdown (2026-07-17). The aggregate query keeps all child
// rows under that root. Sensitive account-identity enrichment has the stronger
// server-owned scope gate below because seat_id is otherwise a request param.
func (h *UsageHandler) PersonalByAgentTotal(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	// Account identity is more sensitive than aggregate usage. Only Control's
	// direct server-side reader may request it after resolving the owner seat
	// from the authenticated account. The browser-facing facade strips this
	// marker, so a caller cannot widen scope by editing seat_id in DevTools.
	p.IncludeLastRoute = r.URL.Query().Get("include_last_route") == "true" && r.Header.Get(resolvedOwnerScopeHeader) == "1"
	data, err := h.repo.PersonalByAgentTotal(r.Context(), p)
	if err != nil {
		slog.Error("PersonalByAgentTotal query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.AgentTotal{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// GET /v1/usage/personal/by-session/total?seat_id=...&start_date=...&end_date=...&limit=10
// (or account_id / org_id=personal)
//
// Returns top N (default 10) sessions ranked by total_tokens for the
// 2026-05-26 /user/performance "Top N sessions" chart. session_id ""
// (no session header) is grouped into a single bucket — frontend
// renders it as "(no session)" so users see how much of their traffic
// lacks the session dimension.
//
// Unlike other personal endpoints, ?session_id= filter is ignored
// here (see usage.QueryParams.SessionID doc). ?limit= controls N
// (default 10, no hard cap — the underlying query already coalesces
// per-session so payload size is bounded by distinct sessions).
func (h *UsageHandler) PersonalBySessionTotal(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	if lim := r.URL.Query().Get("limit"); lim != "" {
		if n, perr := strconv.Atoi(lim); perr == nil && n > 0 {
			p.Limit = n
		}
	}
	data, err := h.repo.PersonalBySessionTotal(r.Context(), p)
	if err != nil {
		slog.Error("PersonalBySessionTotal query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.SessionTotal{}
	}
	shared.JSON(w, http.StatusOK, data)
}

// GET /v1/usage/personal/recent?seat_id=...&limit=5  (or account_id / org_id=personal)
// Returns the most recent non-canary requests as raw usage_event_ods
// rows. Default limit 5, capped at 50 to keep the response small —
// callers wanting paginated history should use a dedicated history
// endpoint (not this one, which is shaped for the Overview "Recent
// Requests" mini-card). Phase 3B R23.
func (h *UsageHandler) PersonalRecent(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	// Override the generic Limit (which defaults to 50 in Defaults())
	// with the smaller "recent card" default so callers without
	// ?limit= get a reasonable size payload.
	limit := 5
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 50 {
		limit = 50
	}
	p.Limit = limit

	data, err := h.repo.PersonalRecent(r.Context(), p)
	if err != nil {
		slog.Error("PersonalRecent query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.RecentRequest{}
	}
	shared.JSON(w, http.StatusOK, map[string]any{"requests": data})
}

// GET /v1/usage/personal/detail?seat_id=...&start_date=&end_date=&filter=unpriced&model=&key=&session_id=
// Per-request rows for the Usage Detail page (last 7 days via start/end_date;
// optional drill-down filters). Reads usage_event_ods (per-event source).
func (h *UsageHandler) PersonalUsageDetail(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	q := r.URL.Query()
	p.Model = q.Get("model")
	p.VirtualKeyID = q.Get("key")
	p.Protocol = q.Get("protocol")
	p.OAuthIdentity = q.Get("identity")
	p.Unpriced = q.Get("filter") == "unpriced"
	limit := 500
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	p.Limit = limit

	data, err := h.repo.PersonalUsageDetail(r.Context(), p)
	if err != nil {
		slog.Error("PersonalUsageDetail query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.UsageDetailRow{}
	}
	shared.JSON(w, http.StatusOK, map[string]any{"rows": data})
}

// GET /v1/usage/personal/by-model/total?seat_id=...&start_date=...&end_date=...
// OR account_id= / org_id=personal
//
// Powers `/user/cost` "Usage by model". Returns rows sorted by
// total_tokens DESC, capped at 20 in the repo layer. Model strings
// are kept as provider-reported (no snapshot normalization).
func (h *UsageHandler) PersonalByModelTotal(w http.ResponseWriter, r *http.Request) {
	p, err := parsePersonalParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	data, err := h.repo.PersonalByModelTotal(r.Context(), p)
	if err != nil {
		slog.Error("PersonalByModelTotal query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.ModelTotal{}
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

// parseMasterAuditFilters reads the optional audit filter dimensions
// (20260729 用量审计页自由筛选) shared by MasterUsageDetail and
// MasterUsageExport — one parser for both entry points so the on-screen rows
// and the exported CSV always narrow by the same rules. Param names reuse the
// personal detail vocabulary where the column is the same (model / key /
// protocol). Returns an error for an invalid `priced` value instead of
// silently ignoring it (a typo must not render an unfiltered audit view).
func parseMasterAuditFilters(p *usage.QueryParams, r *http.Request) error {
	q := r.URL.Query()
	p.SeatID = q.Get("seat_id")
	p.CredentialID = q.Get("credential_id")
	p.ProviderCode = q.Get("provider")
	p.Model = q.Get("model")
	p.QualityStatus = q.Get("quality")
	p.VirtualKeyID = q.Get("key")
	p.Protocol = q.Get("protocol")
	p.AnomalyType = q.Get("anomaly")
	switch b := q.Get("priced"); b {
	case "", "priced", "unpriced":
		p.Billing = b
	default:
		return fmt.Errorf("priced must be 'priced' or 'unpriced', got %q", b)
	}
	return nil
}

// GET /v1/usage/master/detail?org_id=...&days=3&limit=1000
//   or ...&start_date=YYYY-MM-DD&end_date=YYYY-MM-DD (≤31-day span)
//   plus optional filters: seat_id / credential_id / provider / model /
//   quality / key / protocol / anomaly / priced (see parseMasterAuditFilters).
//
// Per-event audit rows for the enterprise usage-audit page. Window is either an
// explicit [start_date, end_date] range (20260729 filters — the page's date
// picker, capped at 31 days; older history goes through /export) or the last
// `days` usage_date days (default 3, the pre-filter behaviour kept for
// backward compatibility). usage_date is UTC (matches the projector).
func (h *UsageHandler) MasterUsageDetail(w http.ResponseWriter, r *http.Request) {
	p, err := parseMasterParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	if err := parseMasterAuditFilters(&p, r); err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	q := r.URL.Query()
	if q.Get("start_date") != "" || q.Get("end_date") != "" {
		// Explicit range path. parseMasterParams already parsed valid values
		// into p.Start/EndDate; a zero value here means missing or malformed.
		if p.StartDate.IsZero() || p.EndDate.IsZero() {
			shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", "start_date and end_date must both be YYYY-MM-DD")
			return
		}
		if p.EndDate.Before(p.StartDate) {
			shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", "end_date must not be before start_date")
			return
		}
		if p.EndDate.Sub(p.StartDate) > 31*24*time.Hour {
			shared.Error(w, http.StatusBadRequest, "RANGE_TOO_LARGE", "detail range must not exceed 31 days; use /export for older history")
			return
		}
	} else {
		days := 3
		if d := q.Get("days"); d != "" {
			if n, e := strconv.Atoi(d); e == nil && n > 0 {
				days = n
			}
		}
		if days > 31 {
			days = 31 // hard cap — the page is a "recent" view, not the export
		}
		now := time.Now().UTC()
		p.EndDate = now
		p.StartDate = now.AddDate(0, 0, -(days - 1))
	}
	limit := 1000
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, e := strconv.Atoi(l); e == nil && n > 0 {
			limit = n
		}
	}
	if limit > 5000 {
		limit = 5000
	}
	p.Limit = limit

	data, err := h.repo.MasterUsageDetail(r.Context(), p)
	if err != nil {
		slog.Error("MasterUsageDetail query failed", "error", err)
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if data == nil {
		data = []usage.MasterUsageAuditRow{}
	}
	shared.JSON(w, http.StatusOK, map[string]any{"rows": data})
}

// GET /v1/usage/master/export?org_id=...&start_date=YYYY-MM-DD&end_date=YYYY-MM-DD
//   plus the same optional audit filters as /detail (parseMasterAuditFilters).
//
// Streams the full audit column set as CSV for [start_date, end_date] (inclusive
// by usage_date). Both dates are REQUIRED and the span is capped at 366 days so
// an unbounded scan can't be requested. The body streams row-by-row off a DB
// cursor (memory O(1)) so a year-long export never materialises. NULL
// billable_amount is emitted as empty — read it together with pricing_snapshot_id
// (="unpriced", not "no charge"). v1.0.1-alpha.4.
func (h *UsageHandler) MasterUsageExport(w http.ResponseWriter, r *http.Request) {
	p, err := parseMasterParams(r)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	// Same filter set as /detail (20260729): the CSV narrows exactly like the
	// on-screen table, so "what I filtered is what I export" holds.
	if err := parseMasterAuditFilters(&p, r); err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	q := r.URL.Query()
	sd, ed := q.Get("start_date"), q.Get("end_date")
	if sd == "" || ed == "" {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", "start_date and end_date are required")
		return
	}
	start, e1 := time.Parse("2006-01-02", sd)
	end, e2 := time.Parse("2006-01-02", ed)
	if e1 != nil || e2 != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", "start_date/end_date must be YYYY-MM-DD")
		return
	}
	if end.Before(start) {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", "end_date must not be before start_date")
		return
	}
	if end.Sub(start) > 366*24*time.Hour {
		shared.Error(w, http.StatusBadRequest, "RANGE_TOO_LARGE", "export range must not exceed 366 days")
		return
	}
	p.StartDate, p.EndDate = start, end

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="usage-audit-%s-%s_%s.csv"`, p.OrgID, sd, ed))
	cw := csv.NewWriter(w)
	_ = cw.Write(masterAuditCSVHeader)
	flusher, _ := w.(http.Flusher)
	n := 0
	streamErr := h.repo.StreamMasterUsageExport(r.Context(), p, func(a *usage.MasterUsageAuditRow) error {
		if err := cw.Write(masterAuditCSVRecord(a)); err != nil {
			return err
		}
		n++
		if n%500 == 0 { // periodic flush keeps the download progressing for big ranges
			cw.Flush()
			if flusher != nil {
				flusher.Flush()
			}
		}
		return nil
	})
	cw.Flush()
	if flusher != nil {
		flusher.Flush()
	}
	if streamErr != nil {
		// Headers + partial body already sent — can't switch to an error status.
		// Surface in logs so a truncated export is diagnosable.
		slog.Error("MasterUsageExport stream failed", "error", streamErr, "rows_written", n)
	}
}

// masterAuditCSVHeader is the column order for the audit export. en-US / fixed —
// not locale-dependent (code-and-ui-language rule).
var masterAuditCSVHeader = []string{
	"event_id", "event_time", "occurred_at", "usage_date", "billing_period",
	"account_id", "seat_id", "seat_alias", "provider_code", "model", "protocol_type", "route_source",
	"virtual_key_id", "virtual_key_hash", "credential_id", "oauth_identity", "credential_fingerprint", "real_key_hash", "binding_id",
	"input_tokens", "output_tokens", "cached_input_tokens", "cache_creation_input_tokens", "reasoning_tokens", "total_tokens",
	"billable_amount", "currency", "pricing_snapshot_id",
	"quality_status", "validation_code", "anomaly_type", "completion_source",
	"content_hash", "source_id", "source_seq",
}

func masterAuditCSVRecord(a *usage.MasterUsageAuditRow) []string {
	billable := ""
	if a.BillableAmount != nil {
		billable = *a.BillableAmount
	}
	sourceSeq := ""
	if a.SourceSeq != nil {
		sourceSeq = strconv.FormatInt(*a.SourceSeq, 10)
	}
	return []string{
		a.EventID, msToRFC3339(a.EventTimeMs), msToRFC3339(a.OccurredAtMs), a.UsageDate, a.BillingPeriod,
		a.AccountID, a.SeatID, a.SeatAlias, a.ProviderCode, a.Model, a.ProtocolType, a.RouteSource,
		a.VirtualKeyID, a.VirtualKeyHash, a.CredentialID, a.OAuthIdentity, a.CredentialFingerprint, a.RealKeyHash, a.BindingID,
		strconv.FormatInt(a.InputTokens, 10), strconv.FormatInt(a.OutputTokens, 10),
		strconv.FormatInt(a.CachedInputTokens, 10), strconv.FormatInt(a.CacheCreationInputTokens, 10),
		strconv.FormatInt(a.ReasoningTokens, 10), strconv.FormatInt(a.TotalTokens, 10),
		billable, a.Currency, a.PricingSnapshotID,
		a.QualityStatus, a.ValidationCode, a.AnomalyType, a.CompletionSource,
		a.ContentHash, a.SourceID, sourceSeq,
	}
}

// msToRFC3339 renders epoch millis as UTC RFC3339 (locale-independent). 0 → "".
func msToRFC3339(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
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
		// Phase 4 Connected Apps (Stage B): optional per-app scoping for
		// `personalTimeline` + `personalByModelTotal`. Other endpoints
		// ignore this field — see QueryParams.AppSlug doc.
		AppSlug: q.Get("app_slug"),
		// Performance drill-down (2026-05-26): optional per-session
		// scoping. Honored by personalByKeyTotal + personalByModelTotal
		// when set; personalBySessionTotal deliberately ignores it (the
		// Top N ranking shouldn't collapse to one row when a session
		// is selected). See QueryParams.SessionID doc.
		SessionID: q.Get("session_id"),
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
