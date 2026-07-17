package api

import (
	"database/sql"
	"net/http"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/pkg/buildinfo"
)

// NewRouter builds the query-service HTTP handler.
//
// db is used only for the /internal/canary-check liveness endpoint. Nil is
// tolerated for tests that don't exercise that path.
func NewRouter(h *UsageHandler, admin *AdminHandler, conv *ConversationHandler, db *sql.DB, serviceToken string) http.Handler {
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		shared.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Version — unauthenticated
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildinfo.Get().JSON())
	})

	// Query-stage canary check — unauthenticated liveness endpoint read by
	// the proxy's Canary probe. See canary.go for why this is distinct from
	// collector-service's /internal/canary-check.
	if db != nil {
		mux.HandleFunc("GET /internal/canary-check", HandleQueryCanaryCheck(db))
	}

	// Authenticated query endpoints
	authed := http.NewServeMux()

	// Personal page
	authed.HandleFunc("GET /v1/usage/personal/timeline", h.PersonalTimeline)
	authed.HandleFunc("GET /v1/usage/personal/hourly", h.PersonalHourlyTimeline)
	authed.HandleFunc("GET /v1/usage/personal/by-protocol/timeline", h.PersonalByProtocolTimeline)
	// 2026-05-28 — "1D" range option uses hourly per-protocol stack
	// instead of daily. Single-day window via ?date= parameter.
	authed.HandleFunc("GET /v1/usage/personal/by-protocol/hourly", h.PersonalByProtocolHourly)
	authed.HandleFunc("GET /v1/usage/personal/by-protocol/total", h.PersonalByProtocolTotal)
	authed.HandleFunc("GET /v1/usage/personal/by-key/total", h.PersonalByKeyTotal)
	authed.HandleFunc("GET /v1/usage/personal/by-model/total", h.PersonalByModelTotal)
	// 2026-05-25 — "Usage By App" ranked-bar chart on /user/usage-ledger.
	// See usage.AppTotal + repository_sql.PersonalByAppTotal for the
	// (app_slug, provider_code) grouping rationale.
	authed.HandleFunc("GET /v1/usage/personal/by-app/total", h.PersonalByAppTotal)
	// 2026-07-17 — "Usage By Agent" breakdown on /user/usage-ledger. Groups by
	// seat_id for the caller + their Agent seats (parent_seat_id). See
	// usage.AgentTotal + repository_sql.PersonalByAgentTotal. Authorization is
	// server-side (caller only sees own + owned agents).
	authed.HandleFunc("GET /v1/usage/personal/by-agent/total", h.PersonalByAgentTotal)
	// 2026-05-26 — "Top N sessions" ranked chart on /user/performance.
	// See usage.SessionTotal + repository_sql.PersonalBySessionTotal for
	// per-session grouping. Default N=10, override with ?limit=N.
	authed.HandleFunc("GET /v1/usage/personal/by-session/total", h.PersonalBySessionTotal)
	// Phase 3B R23 (2026-05-11): raw recent requests for Overview card.
	authed.HandleFunc("GET /v1/usage/personal/recent", h.PersonalRecent)
	// Usage Detail page (2026-06-05): per-request rows, last 7 days, drill-down.
	authed.HandleFunc("GET /v1/usage/personal/detail", h.PersonalUsageDetail)

	// Master page
	authed.HandleFunc("GET /v1/usage/master/ranking", h.MasterUserRanking)
	authed.HandleFunc("GET /v1/usage/master/by-protocol/total", h.MasterByProtocolTotal)
	authed.HandleFunc("GET /v1/usage/master/timeline", h.MasterTimeline)
	// Enterprise usage-audit (v1.0.1-alpha.4): recent per-event detail + full
	// CSV export. detail = last N days (default 3) on usage_date; export streams
	// a ≤366-day range as CSV. Reads usage_fact_dwd (audit columns incl. the
	// content_hash/source_id/source_seq projected in v1.0.1-alpha.3).
	authed.HandleFunc("GET /v1/usage/master/detail", h.MasterUsageDetail)
	authed.HandleFunc("GET /v1/usage/master/export", h.MasterUsageExport)

	// Admin (cost-pricing Stage 3) — pending-pricing queue + per-event
	// audit. Same service-token auth as above; user admin-role gating is
	// enforced upstream in aikey-control (Stage 6). admin may be nil in
	// tests that don't exercise these routes.
	if admin != nil {
		authed.HandleFunc("GET /v1/admin/unpriced-models", admin.ListUnpricedModels)
		authed.HandleFunc("POST /v1/admin/unpriced-models/{provider}/{model}", admin.UpdateUnpricedModelStatus)
		authed.HandleFunc("GET /v1/admin/events/{event_id}/audit", admin.GetEventAudit)
	}

	// Enterprise conversation-audit read views (seat list → session list →
	// thread drawer). The master console facades /v1/conversation-audit/* here
	// (mirrors the usage facade); org_id arrives as a query param. conv may be
	// nil in tests that don't exercise these routes.
	if conv != nil {
		authed.HandleFunc("GET /v1/conversation-audit/seats", conv.Seats)
		authed.HandleFunc("GET /v1/conversation-audit/sessions", conv.Sessions)
		authed.HandleFunc("GET /v1/conversation-audit/thread", conv.Thread)
		authed.HandleFunc("GET /v1/conversation-audit/export", conv.Export)
	}

	mux.Handle("/v1/", shared.ServiceTokenAuth(serviceToken, authed))

	return mux
}
