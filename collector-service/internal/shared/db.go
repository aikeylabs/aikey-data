// Package shared provides cross-cutting utilities for the collector service.
package shared

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "github.com/lib/pq"
)

// Connection-pool defaults, overridable via env (P0-4 capacity review found
// the hardcoded 20 queues 300-concurrent ingest in the Go pool before
// PostgreSQL is even reached). Defaults unchanged; env names follow the
// AIKEY_ prefix convention (config-split §SR8, like AIKEY_LOG_LEVEL).
const (
	defaultDBMaxOpenConns = 20
	defaultDBMaxIdleConns = 5
)

// envPoolSize reads a positive integer pool size from env; empty/invalid →
// fallback with a WARN (never silently — logging-conventions).
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

// OpenDB opens a PostgreSQL connection and verifies connectivity.
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

// RunMigrations executes unapplied .sql files from the given directory in lexical order.
// It tracks applied migrations in a `schema_migrations` table so each migration runs only once.
// Each migration runs in its own transaction; if any fails, the process stops.
func RunMigrations(db *sql.DB, dir string) error {
	if err := ensureMigrationsTable(db); err != nil {
		return err
	}

	applied, err := getAppliedMigrations(db)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %s: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, f := range files {
		if applied[f] {
			continue
		}

		path := filepath.Join(dir, f)
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for migration %s: %w", f, err)
		}

		if _, err := tx.Exec(string(content)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", f, err)
		}

		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (filename) VALUES ($1)`, f,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", f, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", f, err)
		}

		slog.Info("migration applied", "file", f)
	}
	return nil
}

func ensureMigrationsTable(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}
	return nil
}

func getAppliedMigrations(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT filename FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan migration row: %w", err)
		}
		applied[name] = true
	}
	return applied, rows.Err()
}
