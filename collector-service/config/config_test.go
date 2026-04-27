package config

import "testing"

// Pins the AIKEY_LOG_LEVEL contract introduced in reviewer round 5 F1.
// Previously read bare LOG_LEVEL; docs (scheme §SR8 + READMEs) always
// said AIKEY_LOG_LEVEL. If a future refactor accidentally reverts to
// the bare name these tests fail loudly.
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
