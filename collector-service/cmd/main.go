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
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/projector"
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

	// Assemble dependencies — ingest
	odsRepo := ingest.NewSQLODSRepository(ddb)
	ingestSvc := ingest.NewService(odsRepo)
	ingestHandler := api.NewIngestHandler(ingestSvc)

	// Assemble dependencies — projector
	odsReader := projector.NewSQLODSReader(ddb)
	dwdWriter := projector.NewSQLDWDWriter(ddb)
	checkpoint := projector.NewSQLCheckpointStore(ddb)
	controlReader := projector.NewSQLControlEventReader(ddb)
	enricher := projector.NewEnricher(controlReader)
	projWorker := projector.NewWorker(odsReader, dwdWriter, checkpoint, enricher)

	router := api.NewRouter(ingestHandler, ingestSvc, projWorker, db, cfg.ServiceToken)

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
