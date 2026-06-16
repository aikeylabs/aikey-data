package api

import (
	"net/http"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/diagnostics"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/integrity"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/projector"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/buildinfo"
)

// MetricsResponse combines ingest and projector metrics.
type MetricsResponse struct {
	Ingest    ingest.MetricsSnapshot          `json:"ingest"`
	Projector projector.WorkerMetricsSnapshot `json:"projector"`
}

// NewRouter creates the HTTP route multiplexer.
//
// db is used for diagnostics endpoints (/v1/diagnostics/pipeline and
// /internal/canary-check). It MUST be the dialect-aware *shared.DB (which
// rewrites ? → $1 for PostgreSQL), NOT a raw *sql.DB: HandleCanaryCheck's
// `WHERE event_id = ?` silently errored on PG (canary-check returned
// ods_received:false for rows that were actually present), making the cluster
// usage-pipeline health permanently red while real ingest worked fine. SQLite
// hid it (? is native there). Bug: 2026-06-13-cluster-canary-check-pg-placeholder.
// These are kept unauthenticated because:
//   - they are read-only and return pipeline freshness metadata, no business data
//   - the proxy's canary probe hits /internal/canary-check without a bearer token
//   - aikey doctor (CLI) reads /v1/diagnostics/pipeline without a bearer token
//
// If you need to restrict them, wrap both with shared.IngestAuth and
// update the proxy/CLI callers accordingly.
//
// jwtSecret + serviceToken together drive shared.IngestAuth:
//   - JWT path (preferred): bearer is a HS256 JWT signed by control-service
//     with the same secret; account_id from claims is auto-attached to
//     ingest request context for force-overwrite (see HandleBatch).
//   - service_token path (escape hatch): legacy S2S token; client may
//     claim any account_id in event payloads.
//
// Either may be empty. Both empty = open ingest (dev / CI default).
// gapScanner (optional) enables GET /v1/diagnostics/completeness — the
// delivery-integrity per-source completeness view. nil → endpoint not mounted
// (e.g. a build without watermark schema). Same dialect-aware scanner instance
// the projector's detection loop uses (single source of truth for "what is a gap").
func NewRouter(ingestH *IngestHandler, ingestSvc *ingest.Service, convH *ConversationHandler, projWorker *projector.Worker, db diagnostics.DBQuerier, gapScanner *integrity.Scanner, deliveryRepo gapsRepo, jwtSecret []byte, serviceToken string) http.Handler {
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
	// Delivery-integrity completeness — read-only, unauthenticated like the
	// sibling diagnostics endpoints. The more-specific GET pattern takes
	// precedence over the "/v1/" auth mount below (Go 1.22+ ServeMux), matching
	// how /v1/diagnostics/pipeline coexists with the authed /v1/ tree.
	if gapScanner != nil {
		mux.HandleFunc("GET /v1/diagnostics/completeness", handleCompleteness(gapScanner))
		// reconcile (D2): force a scan + known-loss promotion, return the settled
		// view. POST (state-changing: may promote stale gaps to the ledger).
		mux.HandleFunc("POST /v1/diagnostics/reconcile", handleReconcile(projWorker))
	}
	// D3 client-confirmed reconciliation: enumerate a source's gaps (so a client
	// can check its WAL) + accept client "confirm lost" for genuinely-absent seqs.
	if deliveryRepo != nil {
		mux.HandleFunc("GET /v1/diagnostics/gaps", handleGaps(deliveryRepo))
		mux.HandleFunc("POST /v1/diagnostics/confirm-lost", handleConfirmLost(deliveryRepo))
	}

	// Ingest API — authenticated
	authed := http.NewServeMux()
	authed.HandleFunc("POST /v1/usage-events:batch", ingestH.HandleBatch)
	// Conversation audit (v1.0.1-alpha.2): content records, same auth mount.
	authed.HandleFunc("POST /v1/conversation-records:batch", convH.HandleBatch)

	mux.Handle("/v1/", shared.IngestAuth(jwtSecret, serviceToken, authed))

	return mux
}
