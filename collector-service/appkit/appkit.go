// Package appkit exports a factory function that assembles the collector-service
// into a single http.Handler. Used by aikey-trial-server to embed the collector
// without importing internal/ packages directly.
package appkit

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/api"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/integrity"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/projector"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/buildinfo"
)

// Config holds configuration for the collector-service handler assembly.
type Config struct {
	// DBDialect selects placeholder rewriting: "postgres" or "sqlite".
	DBDialect string
	// ServiceToken is the legacy single-token bearer for ingest. Kept as
	// an admin escape hatch — when set, the collector accepts a matching
	// Authorization header AND trusts client-claimed account_id verbatim.
	// Preferred path is JWTSecret (per-user JWT, server-side account_id
	// overwrite). See roadmap update 20260511-user-jwt-collector-ingest.md.
	ServiceToken string
	// JWTSecret is the HS256 signing secret control-service uses to issue
	// user access tokens. The collector verifies inbound bearers against
	// this. When set, ingest requests carrying a valid JWT have the
	// JWT's account_id force-overwritten onto every event in the batch.
	// Empty means "no JWT path; only ServiceToken accepted".
	JWTSecret []byte
	// Logger for collector output.
	Logger *slog.Logger
}

// Result holds the assembled handler and background worker.
type Result struct {
	Handler      http.Handler
	RunProjector func(ctx context.Context)
}

// New assembles the collector-service and returns its HTTP handler plus
// a projector worker function. The caller provides an opened, migrated DB.
func New(db *sql.DB, cfg Config) Result {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// 2026-05-11 B-phase admin-escape-hatch surfacing: legacy
	// ServiceToken is still accepted but is an admin trust-mode path
	// (event payloads' account_id is honoured verbatim). Loud startup
	// log so operators see they've enabled the S2S fallback.
	if cfg.ServiceToken != "" {
		logger.Warn(
			"ingest auth: service_token fallback ENABLED — clients holding this token can forge events for any account. Prefer per-user JWT (set JWTSecret + clear ServiceToken).",
			"event.name", "ingest.auth.service_token_enabled",
		)
	}

	ddb := shared.NewDB(db, cfg.DBDialect)

	odsRepo := ingest.NewSQLODSRepository(ddb)
	ingestSvc := ingest.NewService(odsRepo)
	ingestHandler := api.NewIngestHandler(ingestSvc)

	odsReader := projector.NewSQLODSReader(ddb)
	dwdWriter := projector.NewSQLDWDWriter(ddb)
	checkpoint := projector.NewSQLCheckpointStore(ddb)
	ctrlReader := projector.NewSQLControlEventReader(ddb)
	// Cost-pricing resolver (v1.0.0-rc.8): embedded price table loaded once.
	// Trial/Personal assemble in-process and this func returns no error, so a
	// load failure (corrupt embedded file — unrecoverable) panics = fail-fast,
	// matching the production collector's os.Exit(1) (design §2.3: never run
	// with an empty price table).
	pricingResolver, perr := pricing.Load()
	if perr != nil {
		logger.Error("load pricing resolver", "error", perr)
		panic("pricing resolver load failed: " + perr.Error())
	}
	logger.Info("pricing resolver loaded",
		"event.name", "pricing.resolver.loaded",
		"snapshot_id", pricingResolver.Snapshot().SnapshotID)
	enricher := projector.NewEnricher(ctrlReader, pricingResolver)
	// Stage 2.4: async unpriced-models queue + active-snapshot record (audit
	// anchor). Snapshot ensure is non-fatal — cost still computes; a missing
	// snapshot row only degrades audit and must not block assembly.
	unpricedQueue := projector.NewUnpricedQueue(projector.NewSQLUnpricedRepository(ddb))
	enricher.SetUnpricedSink(unpricedQueue)
	if serr := projector.NewSQLSnapshotRepository(ddb).EnsureSnapshot(
		context.Background(), pricingResolver.Snapshot(), buildinfo.Get().Version, time.Now().UnixMilli(),
	); serr != nil {
		logger.Warn("ensure pricing snapshot",
			"event.name", "pricing.snapshot.ensure_failed", "error", serr)
	}
	projWorker := projector.NewWorker(odsReader, dwdWriter, checkpoint, enricher)
	// Delivery-integrity: one scanner instance drives BOTH the projector's
	// periodic WARN surface and the /v1/diagnostics/completeness endpoint.
	gapScanner := integrity.NewScanner(ddb, integrity.DefaultCriteria())
	projWorker.SetGapScanner(gapScanner)
	// Stage D1: the ingest repo doubles as the known-loss LossPromoter so the
	// detection tick promotes stale gaps to the ledger + advances contiguous.
	projWorker.SetLossPromoter(odsRepo)

	router := api.NewRouter(ingestHandler, ingestSvc, projWorker, db, gapScanner, odsRepo, cfg.JWTSecret, cfg.ServiceToken)

	return Result{
		Handler: router,
		// RunProjector also drives the unpriced-models worker on the same ctx, so
		// the embedding caller (trial/personal) starts both with one call and
		// they stop together on shutdown.
		RunProjector: func(ctx context.Context) {
			go unpricedQueue.Run(ctx)
			projWorker.Run(ctx)
		},
	}
}
