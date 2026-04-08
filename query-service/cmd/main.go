// query-service: provides usage query APIs for personal and master dashboards.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/config"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/api"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

func main() {
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

	repo := usage.NewPostgresRepository(shared.NewDB(db, shared.DialectPostgres))
	handler := api.NewUsageHandler(repo)
	router := api.NewRouter(handler, cfg.ServiceToken)

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("query-service started", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("listen", "error", err)
			os.Exit(1)
		}
	}()

	<-done
	slog.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown", "error", err)
	}
	slog.Info("query-service stopped")
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
