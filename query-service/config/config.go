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
		ListenAddr:   envOrDefault("LISTEN_ADDR", "0.0.0.0:27301"),
		ServiceToken: os.Getenv("SERVICE_TOKEN"),
		LogLevel:     envOrDefault("LOG_LEVEL", "info"),
	}, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
