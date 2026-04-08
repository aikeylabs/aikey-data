// Package appkit exports a factory function that assembles the collector-service
// into a single http.Handler. Used by aikey-trial-server to embed the collector
// without importing internal/ packages directly.
package appkit

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/api"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/projector"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// Config holds configuration for the collector-service handler assembly.
type Config struct {
	// DBDialect selects placeholder rewriting: "postgres" or "sqlite".
	DBDialect string
	// ServiceToken is the bearer token for authenticated ingest endpoints.
	ServiceToken string
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

	ddb := shared.NewDB(db, cfg.DBDialect)

	odsRepo := ingest.NewPostgresODSRepository(ddb)
	ingestSvc := ingest.NewService(odsRepo)
	ingestHandler := api.NewIngestHandler(ingestSvc)

	odsReader := projector.NewPostgresODSReader(ddb)
	dwdWriter := projector.NewPostgresDWDWriter(ddb)
	checkpoint := projector.NewPostgresCheckpointStore(ddb)
	ctrlReader := projector.NewPostgresControlEventReader(ddb)
	enricher := projector.NewEnricher(ctrlReader)
	projWorker := projector.NewWorker(odsReader, dwdWriter, checkpoint, enricher)

	router := api.NewRouter(ingestHandler, ingestSvc, projWorker, cfg.ServiceToken)

	return Result{
		Handler:      router,
		RunProjector: projWorker.Run,
	}
}
