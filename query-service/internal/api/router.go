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
func NewRouter(h *UsageHandler, db *sql.DB, serviceToken string) http.Handler {
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
	authed.HandleFunc("GET /v1/usage/personal/by-protocol/total", h.PersonalByProtocolTotal)
	authed.HandleFunc("GET /v1/usage/personal/by-key/total", h.PersonalByKeyTotal)
	authed.HandleFunc("GET /v1/usage/personal/by-model/total", h.PersonalByModelTotal)
	// 2026-05-25 — "Usage By App" ranked-bar chart on /user/usage-ledger.
	// See usage.AppTotal + repository_sql.PersonalByAppTotal for the
	// (app_slug, provider_code) grouping rationale.
	authed.HandleFunc("GET /v1/usage/personal/by-app/total", h.PersonalByAppTotal)
	// 2026-05-26 — "Top N sessions" ranked chart on /user/performance.
	// See usage.SessionTotal + repository_sql.PersonalBySessionTotal for
	// per-session grouping. Default N=10, override with ?limit=N.
	authed.HandleFunc("GET /v1/usage/personal/by-session/total", h.PersonalBySessionTotal)
	// Phase 3B R23 (2026-05-11): raw recent requests for Overview card.
	authed.HandleFunc("GET /v1/usage/personal/recent", h.PersonalRecent)

	// Master page
	authed.HandleFunc("GET /v1/usage/master/ranking", h.MasterUserRanking)
	authed.HandleFunc("GET /v1/usage/master/by-protocol/total", h.MasterByProtocolTotal)
	authed.HandleFunc("GET /v1/usage/master/timeline", h.MasterTimeline)

	mux.Handle("/v1/", shared.ServiceTokenAuth(serviceToken, authed))

	return mux
}
