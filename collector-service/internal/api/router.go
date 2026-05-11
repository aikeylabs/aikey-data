package api

import (
	"database/sql"
	"net/http"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/diagnostics"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/projector"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/buildinfo"
)

// MetricsResponse combines ingest and projector metrics.
type MetricsResponse struct {
	Ingest    ingest.MetricsSnapshot           `json:"ingest"`
	Projector projector.WorkerMetricsSnapshot   `json:"projector"`
}

// NewRouter creates the HTTP route multiplexer.
//
// db is used for diagnostics endpoints (/v1/diagnostics/pipeline and
// /internal/canary-check). These are kept unauthenticated because:
//   - they are read-only and return pipeline freshness metadata, no business data
//   - the proxy's canary probe hits /internal/canary-check without a bearer token
//   - aikey doctor (CLI) reads /v1/diagnostics/pipeline without a bearer token
// If you need to restrict them, wrap both with shared.IngestAuth and
// update the proxy/CLI callers accordingly.
//
// jwtSecret + serviceToken together drive shared.IngestAuth:
//   - JWT path (preferred): bearer is a HS256 JWT signed by control-service
//     with the same secret; account_id from claims is auto-attached to
//     ingest request context for force-overwrite (see HandleBatch).
//   - service_token path (escape hatch): legacy S2S token; client may
//     claim any account_id in event payloads.
// Either may be empty. Both empty = open ingest (dev / CI default).
func NewRouter(ingestH *IngestHandler, ingestSvc *ingest.Service, projWorker *projector.Worker, db *sql.DB, jwtSecret []byte, serviceToken string) http.Handler {
	mux := http.NewServeMux()

	// Health check — unauthenticated
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		shared.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Version — unauthenticated. Includes supported event schema versions
	// so proxy can check compatibility when versions are mismatched.
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		info := buildinfo.Get()
		resp := struct {
			buildinfo.Info
			SupportedEventSchemaVersions []int `json:"supported_event_schema_versions"`
		}{
			Info:                         info,
			SupportedEventSchemaVersions: ingest.SupportedSchemaVersions,
		}
		shared.JSON(w, http.StatusOK, resp)
	})

	// Metrics — unauthenticated
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		shared.JSON(w, http.StatusOK, MetricsResponse{
			Ingest:    ingestSvc.MetricsSnapshot(),
			Projector: projWorker.MetricsSnapshot(),
		})
	})

	// Diagnostics — unauthenticated. Canonical home for pipeline observability;
	// trial-server delegates these paths to this handler, production container
	// exposes them on its own listen address.
	if db != nil {
		mux.HandleFunc("GET /v1/diagnostics/pipeline", diagnostics.HandlePipeline(db))
		mux.HandleFunc("GET /internal/canary-check", diagnostics.HandleCanaryCheck(db))
	}

	// Ingest API — authenticated
	authed := http.NewServeMux()
	authed.HandleFunc("POST /v1/usage-events:batch", ingestH.HandleBatch)

	mux.Handle("/v1/", shared.IngestAuth(jwtSecret, serviceToken, authed))

	return mux
}
