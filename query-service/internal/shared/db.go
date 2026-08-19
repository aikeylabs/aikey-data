package shared

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	_ "github.com/lib/pq"
)

// Pool defaults overridable via env (P0-4 capacity review; mirrors
// collector-service/internal/shared/db.go — AIKEY_ prefix per config-split §SR8).
const (
	defaultDBMaxOpenConns = 20
	defaultDBMaxIdleConns = 5
)

func envPoolSize(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("invalid pool size env; using default",
			"event.name", "shared.db.pool_env_invalid", "env", key, "value", v, "default", fallback)
		return fallback
	}
	return n
}

func OpenDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	db.SetMaxOpenConns(envPoolSize("AIKEY_DB_MAX_OPEN_CONNS", defaultDBMaxOpenConns))
	db.SetMaxIdleConns(envPoolSize("AIKEY_DB_MAX_IDLE_CONNS", defaultDBMaxIdleConns))
	return db, nil
}
