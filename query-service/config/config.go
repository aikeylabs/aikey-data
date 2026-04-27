package config

import (
	"fmt"
	"os"
)

type Config struct {
	DatabaseDSN  string
	ListenAddr   string
	ServiceToken string
	LogLevel     string
}

func Load() (*Config, error) {
	dsn := os.Getenv("DATABASE_DSN")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_DSN is required")
	}
	return &Config{
		DatabaseDSN:  dsn,
		ListenAddr:   envOrDefault("LISTEN_ADDR", "0.0.0.0:27310"),
		ServiceToken: os.Getenv("SERVICE_TOKEN"),
		// Why AIKEY_LOG_LEVEL: see collector-service/config/config.go for full
		// rationale (scheme §SR8 + reviewer round 5 F1 — align code to docs).
		LogLevel:     envOrDefault("AIKEY_LOG_LEVEL", "info"),
	}, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
