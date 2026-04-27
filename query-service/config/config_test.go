package config

import "testing"

// See collector-service/config/config_test.go for full rationale —
// scheme §SR8 + reviewer round 5 F1 contract pin. Identical shape kept
// deliberately so a future env-name change in one service is caught
// here too.
func TestLoad_AIKEYLogLevelEnvOverride(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://x")
	t.Setenv("AIKEY_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("AIKEY_LOG_LEVEL should set log level, got %q", cfg.LogLevel)
	}
}

func TestLoad_BareLogLevelEnvIgnored(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://x")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("AIKEY_LOG_LEVEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("bare LOG_LEVEL must not override default; got %q", cfg.LogLevel)
	}
}
