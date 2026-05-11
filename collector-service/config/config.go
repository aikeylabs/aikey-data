// Package config loads collector-service configuration from environment variables.
package config

import (
	"fmt"
	"os"
)

type Config struct {
	DatabaseDSN   string
	ListenAddr    string
	MigrationsDir string
	ServiceToken  string
	// JWTSecret is shared with control-service so the collector can
	// verify access tokens issued via `aikey login`. Added 2026-05-11
	// for the B-phase user-JWT ingest path (see
	// roadmap20260320/技术实现/update/20260511-user-jwt-collector-ingest.md).
	// Empty means "no JWT ingest path; only ServiceToken accepted".
	JWTSecret     []byte
	LogLevel      string
}

func Load() (*Config, error) {
	dsn := os.Getenv("DATABASE_DSN")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_DSN is required")
	}

	c := &Config{
		DatabaseDSN:   dsn,
		ListenAddr:    envOrDefault("LISTEN_ADDR", "0.0.0.0:27300"),
		MigrationsDir: envOrDefault("MIGRATIONS_DIR", "./migrations"),
		ServiceToken:  os.Getenv("SERVICE_TOKEN"),
		JWTSecret:     []byte(os.Getenv("JWT_SECRET")),
		// Why AIKEY_LOG_LEVEL: scheme config-split-system-user §SR8 standardized
		// log-level env names on the AIKEY_ prefix. Bare LOG_LEVEL collides too
		// easily with other tools sharing the same container/shell. Reviewer
		// round 5 F1: docs were always AIKEY_LOG_LEVEL but code read bare
		// LOG_LEVEL — this aligns code to the documented contract.
		LogLevel:      envOrDefault("AIKEY_LOG_LEVEL", "info"),
	}
	return c, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
