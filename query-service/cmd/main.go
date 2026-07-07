// query-service: provides usage query APIs for personal and master dashboards.
package main

import (
	// Embed IANA tzdata so time.LoadLocation works on Windows (no system
	// zoneinfo). query-service buckets usage by timezone; without this it logs
	// "unknown IANA TZ ... falling back to UTC" and mis-aggregates on Windows.
	// Bugfix: workflow/CI/bugfix/2026-07-06-windows-server-missing-tzdata.md
	_ "time/tzdata"

	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/config"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/api"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/conversation"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
	"github.com/AiKeyLabs/pkg/aikeycompat"
	"github.com/AiKeyLabs/pkg/buildinfo"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		bi := buildinfo.Get()
		if len(os.Args) > 2 && (os.Args[2] == "--json" || os.Args[2] == "-j") {
			fmt.Println(string(bi.JSON()))
		} else {
			fmt.Println("query-service", bi.String())
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

	ddb := shared.NewDB(db, shared.DialectPostgres)
	repo := usage.NewSQLRepository(ddb)
	handler := api.NewUsageHandler(repo)
	admin := api.NewAdminHandler(repo)
	convH := api.NewConversationHandler(conversation.NewSQLRepository(ddb))
	router := api.NewRouter(handler, admin, convH, db, cfg.ServiceToken)

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, aikeycompat.ShutdownSignals()...)

	go func() {
		slog.Info("query-service started", "addr", cfg.ListenAddr, "version", buildinfo.Get().String())
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
