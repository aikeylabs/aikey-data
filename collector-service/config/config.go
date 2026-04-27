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
