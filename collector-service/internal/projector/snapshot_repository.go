package projector

import (
	"context"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// SnapshotRepository persists the pricing-source fingerprint so every DWD
// event's pricing_snapshot_id resolves to a real row, keeping the cost audit
// chain intact after price files change (design §3.7).
type SnapshotRepository interface {
	EnsureSnapshot(ctx context.Context, snap pricing.Snapshot, aikeyVersion string, now int64) error
}

type sqlSnapshotRepo struct{ db *shared.DB }

// NewSQLSnapshotRepository builds the pricing_snapshots repository.
func NewSQLSnapshotRepository(db *shared.DB) SnapshotRepository {
	return &sqlSnapshotRepo{db: db}
}

// EnsureSnapshot records the current pricing snapshot if new, then closes the
// previously-active one. Idempotent + multi-replica safe: ON CONFLICT DO NOTHING
// makes a repeat a no-op, and only the caller that actually inserted the new row
// (RowsAffected > 0) closes the old active row — an optimistic guard
// (effective_until IS NULL AND snapshot_id != new) avoids a distributed lock.
// Snapshot rows are never deleted; a superseded row just gets effective_until.
func (r *sqlSnapshotRepo) EnsureSnapshot(ctx context.Context, snap pricing.Snapshot, aikeyVersion string, now int64) error {
	const ins = `INSERT INTO pricing_snapshots
	    (snapshot_id, litellm_sha256, history_sha256, overrides_sha256, aikey_version, created_at, effective_from, effective_until)
	VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
	ON CONFLICT (snapshot_id) DO NOTHING`
	res, err := r.db.ExecContext(ctx, ins,
		snap.SnapshotID, snap.LiteLLMSHA256, snap.HistorySHA256, snap.OverridesSHA256,
		aikeyVersion, now, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // snapshot already present → nothing to close
	}
	// New snapshot: mark the prior active snapshot superseded.
	_, err = r.db.ExecContext(ctx,
		`UPDATE pricing_snapshots SET effective_until = ? WHERE effective_until IS NULL AND snapshot_id != ?`,
		now, snap.SnapshotID)
	return err
}
