package api

import (
	"net/http"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/projector"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// MetricsResponse combines ingest and projector metrics.
type MetricsResponse struct {
	Ingest    ingest.MetricsSnapshot           `json:"ingest"`
	Projector projector.WorkerMetricsSnapshot   `json:"projector"`
}

// NewRouter creates the HTTP route multiplexer.
func NewRouter(ingestH *IngestHandler, ingestSvc *ingest.Service, projWorker *projector.Worker, serviceToken string) http.Handler {
	mux := http.NewServeMux()

	// Health check — unauthenticated
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		shared.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Metrics — unauthenticated
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		shared.JSON(w, http.StatusOK, MetricsResponse{
			Ingest:    ingestSvc.MetricsSnapshot(),
			Projector: projWorker.MetricsSnapshot(),
		})
	})

	// Ingest API — authenticated
	authed := http.NewServeMux()
	authed.HandleFunc("POST /v1/usage-events:batch", ingestH.HandleBatch)

	mux.Handle("/v1/", shared.ServiceTokenAuth(serviceToken, authed))

	return mux
}
