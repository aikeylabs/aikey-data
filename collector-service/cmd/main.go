// collector-service: receives usage events from Local Proxy, persists to ODS,
// and asynchronously projects to DWD.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/config"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/api"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/integrity"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/partition"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/projector"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/quota"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeycompat"
	"github.com/AiKeyLabs/pkg/buildinfo"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		bi := buildinfo.Get()
		if len(os.Args) > 2 && (os.Args[2] == "--json" || os.Args[2] == "-j") {
			fmt.Println(string(bi.JSON()))
		} else {
			fmt.Println("collector-service", bi.String())
		}
		os.Exit(0)
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	setupLogger(cfg.LogLevel)

	db, err := shared.OpenDB(cfg.DatabaseDSN)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Apply schema migrations via the unified registry (D plan).
	// Production collector-service owns ONLY the data component's
	// tables (usage_event_ods / usage_fact_dwd / projector tasks).
	//
	// 4-point readiness contract:
	//   1. UpgradeComponentsTo runs SYNCHRONOUSLY before any HTTP listener.
	//   2. Failure is fail-fast os.Exit(1).
	//   3. Health/readiness endpoints register only after this returns nil.
	//   4. Verified by `make verify-boot-order`.
	if err := versions.UpgradeComponentsTo(context.Background(), db, dbmigrate.DialectPostgres,
		[]dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		slog.Error("apply schema migrations", "error", err)
		os.Exit(1)
	}

	// Wrap DB with dialect (production always uses postgres).
	ddb := shared.NewDB(db, shared.DialectPostgres)

	// Ensure the current + next monthly partitions exist for the two
	// partitioned usage tables (v1.0.1-alpha.4 enterprise usage-audit). Non-fatal:
	// the baseline DEFAULT partition keeps writes working even if this hiccups, so
	// a partition issue must NOT block the collector (stability > pruning). ahead=2
	// gives slack against month-boundary restarts.
	for _, p := range []struct{ table, key string }{
		{"usage_fact_dwd", partition.KeyDate},
		{"usage_event_ods", partition.KeyTimestamptz},
	} {
		if err := partition.EnsureMonthlyPartitions(context.Background(), ddb, p.table, p.key, 2, time.Now()); err != nil {
			slog.Warn("ensure monthly partitions",
				"event.name", "partition.ensure_failed", "table", p.table, "error", err)
		}
	}

	// Assemble dependencies — ingest
	odsRepo := ingest.NewSQLODSRepository(ddb)
	ingestSvc := ingest.NewService(odsRepo)
	ingestHandler := api.NewIngestHandler(ingestSvc)

	// Assemble dependencies — projector
	odsReader := projector.NewSQLODSReader(ddb)
	dwdWriter := projector.NewSQLDWDWriter(ddb)
	checkpoint := projector.NewSQLCheckpointStore(ddb)
	// LRU + TTL cache in front of the control-event reader. Projector
	// batches (100 events) frequently share the same virtual_key_id; the
	// inner SQL would otherwise re-fetch the same row per event. See
	// internal/projector/cached_control_reader.go for the design notes.
	// Sizes/TTL chosen for "thousands of active VKs, control changes
	// settle within a few minutes" — operators can tighten the TTL or
	// restart the worker if they need stronger freshness.
	rawControlReader := projector.NewSQLControlEventReader(ddb)
	controlReader, cerr := projector.NewCachedControlEventReader(rawControlReader, 10000, 5*time.Minute)
	if cerr != nil {
		slog.Error("init control-event cache", "error", cerr)
		os.Exit(1)
	}
	// Cost-pricing resolver (v1.0.0-rc.8): load the embedded
	// LiteLLM/history/overrides snapshot once at startup. Fail-fast — an empty
	// price table would silently mark every event unpriced, hiding a
	// misconfiguration (design §2.3).
	pricingResolver, perr := pricing.Load()
	if perr != nil {
		slog.Error("load pricing resolver", "error", perr)
		os.Exit(1)
	}
	slog.Info("pricing resolver loaded",
		"event.name", "pricing.resolver.loaded",
		"snapshot_id", pricingResolver.Snapshot().SnapshotID)
	enricher := projector.NewEnricher(controlReader, pricingResolver)
	// Stage 2.4: wire the async unpriced-models queue + record the active
	// pricing snapshot (audit anchor). Snapshot ensure is non-fatal — cost still
	// computes; a missing snapshot row only degrades the audit trail and must
	// not block the collector (stability > audit completeness).
	unpricedQueue := projector.NewUnpricedQueue(projector.NewSQLUnpricedRepository(ddb))
	enricher.SetUnpricedSink(unpricedQueue)
	if serr := projector.NewSQLSnapshotRepository(ddb).EnsureSnapshot(
		context.Background(), pricingResolver.Snapshot(), buildinfo.Get().Version, time.Now().UnixMilli(),
	); serr != nil {
		slog.Warn("ensure pricing snapshot",
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
	// Phase 2 Stage 5: materialize quota_counter + record threshold crossings as
	// facts project (no-op when no quota_subject rows; best-effort, panic-guarded).
	projWorker.SetQuotaMaterializer(quota.NewMaterializer(quota.NewStorage(ddb), 0))

	router := api.NewRouter(ingestHandler, ingestSvc, projWorker, db, gapScanner, odsRepo, cfg.JWTSecret, cfg.ServiceToken)
	if cfg.ServiceToken != "" {
		slog.Warn(
			"ingest auth: service_token fallback ENABLED — clients holding this token can forge events for any account. Prefer per-user JWT (set JWT_SECRET + clear SERVICE_TOKEN).",
			"event.name", "ingest.auth.service_token_enabled",
		)
	}

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, aikeycompat.ShutdownSignals()...)

	// Start projector worker in background
	projCtx, projCancel := context.WithCancel(context.Background())
	defer projCancel()
	go projWorker.Run(projCtx)
	go unpricedQueue.Run(projCtx)

	go func() {
		slog.Info("collector-service started", "addr", cfg.ListenAddr, "version", buildinfo.Get().String())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("listen", "error", err)
			os.Exit(1)
		}
	}()

	<-done
	slog.Info("shutting down...")

	// Stop projector first
	projCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown", "error", err)
	}
	slog.Info("collector-service stopped")
}

func setupLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
}
