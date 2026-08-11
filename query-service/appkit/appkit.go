// Package appkit exports a factory function that assembles the query-service
// into a single http.Handler. Used by aikey-trial-server to embed the query
// engine without importing internal/ packages directly.
package appkit

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/api"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/conversation"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

// Config holds configuration for the query-service handler assembly.
type Config struct {
	// DBDialect selects placeholder rewriting: "postgres" or "sqlite".
	DBDialect string
	// ServiceToken is the bearer token for authenticated query endpoints.
	ServiceToken string
	// Logger for query-service output.
	Logger *slog.Logger
}

// NewHandler assembles the query-service and returns a single http.Handler.
func NewHandler(db *sql.DB, cfg Config) http.Handler {
	ddb := shared.NewDB(db, cfg.DBDialect)
	repo := usage.NewSQLRepository(ddb)
	handler := api.NewUsageHandler(repo)
	admin := api.NewAdminHandler(repo)
	convH := api.NewConversationHandler(conversation.NewSQLRepository(ddb))
	// Keep embedded Trial/Personal and standalone Production/Cluster on the
	// same dialect-aware query path; SQLite accepts `?`, PostgreSQL needs $N.
	return api.NewRouter(handler, admin, convH, ddb, cfg.ServiceToken)
}
