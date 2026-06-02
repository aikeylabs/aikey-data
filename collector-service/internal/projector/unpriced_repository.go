package projector

import (
	"context"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// UnpricedRepository persists the "pending pricing" queue: one row per
// (model, provider) the resolver could not price, with a running event count
// the dashboard sorts by priority (design §3.4).
type UnpricedRepository interface {
	UpsertUnpriced(ctx context.Context, provider, model string, seenAt int64) error
}

type sqlUnpricedRepository struct{ db *shared.DB }

// NewSQLUnpricedRepository builds the unpriced_models repository.
func NewSQLUnpricedRepository(db *shared.DB) UnpricedRepository {
	return &sqlUnpricedRepository{db: db}
}

// UpsertUnpriced records a pricing miss. First sighting inserts the row; repeats
// bump event_count and last_seen_at. PK is (model, provider); the ON CONFLICT
// clause (incl. `excluded.`) is identical on PG and SQLite 3.24+, so one
// statement serves both dialects (shared.DB rewrites `?` per dialect).
func (r *sqlUnpricedRepository) UpsertUnpriced(ctx context.Context, provider, model string, seenAt int64) error {
	const q = `INSERT INTO unpriced_models (model, provider, first_seen_at, last_seen_at, event_count, status)
	VALUES (?, ?, ?, ?, 1, 'pending')
	ON CONFLICT (model, provider) DO UPDATE SET
	    last_seen_at = excluded.last_seen_at,
	    event_count  = unpriced_models.event_count + 1`
	_, err := r.db.ExecContext(ctx, q, model, provider, seenAt, seenAt)
	return err
}
