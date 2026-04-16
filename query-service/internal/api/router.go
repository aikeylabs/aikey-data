package api

import (
	"net/http"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/pkg/buildinfo"
)

func NewRouter(h *UsageHandler, serviceToken string) http.Handler {
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

	// Authenticated query endpoints
	authed := http.NewServeMux()

	// Personal page
	authed.HandleFunc("GET /v1/usage/personal/timeline", h.PersonalTimeline)
	authed.HandleFunc("GET /v1/usage/personal/by-protocol/timeline", h.PersonalByProtocolTimeline)
	authed.HandleFunc("GET /v1/usage/personal/by-protocol/total", h.PersonalByProtocolTotal)
	authed.HandleFunc("GET /v1/usage/personal/by-key/total", h.PersonalByKeyTotal)

	// Master page
	authed.HandleFunc("GET /v1/usage/master/ranking", h.MasterUserRanking)
	authed.HandleFunc("GET /v1/usage/master/by-protocol/total", h.MasterByProtocolTotal)
	authed.HandleFunc("GET /v1/usage/master/timeline", h.MasterTimeline)

	mux.Handle("/v1/", shared.ServiceTokenAuth(serviceToken, authed))

	return mux
}
