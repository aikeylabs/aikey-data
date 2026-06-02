package projector

import (
	"context"
	"log/slog"
	"time"
)

// UnpricedSink receives (provider, model) pairs the resolver could not price.
// The enricher calls Enqueue on a miss; implementations must be non-blocking.
type UnpricedSink interface {
	Enqueue(provider, model string)
}

type unpricedKey struct{ provider, model string }

// UnpricedQueue is an async, non-blocking sink. Enqueue drops into a buffered
// channel; a background worker UPSERTs unpriced_models. Dropping on a full
// buffer is acceptable — the queue is an audit aid, never on the cost write
// path, so it must never block the projector (design §3.4 / 不变量 6).
type UnpricedQueue struct {
	ch   chan unpricedKey
	repo UnpricedRepository
}

// NewUnpricedQueue builds a queue with a 1000-deep buffer.
func NewUnpricedQueue(repo UnpricedRepository) *UnpricedQueue {
	return &UnpricedQueue{ch: make(chan unpricedKey, 1000), repo: repo}
}

// Enqueue is non-blocking: a full buffer drops + WARNs rather than stalling the
// projector. Implements UnpricedSink.
func (q *UnpricedQueue) Enqueue(provider, model string) {
	select {
	case q.ch <- unpricedKey{provider: provider, model: model}:
	default:
		slog.Warn("unpriced queue full, dropping",
			"event.name", "projector.unpriced.dropped",
			"provider", provider, "model", model)
	}
}

// Run drains the queue until ctx is cancelled. Per-item UPSERT keeps the count
// accurate; UPSERT is idempotent so this stays correct across restart/replay.
// Batching can be added later if miss volume ever warrants it (misses are rare —
// only genuinely unknown models — so per-item DB writes are fine).
func (q *UnpricedQueue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case k := <-q.ch:
			if err := q.repo.UpsertUnpriced(ctx, k.provider, k.model, time.Now().UnixMilli()); err != nil {
				slog.Warn("unpriced upsert failed",
					"event.name", "projector.unpriced.upsert_failed",
					"provider", k.provider, "model", k.model, "error", err)
			}
		}
	}
}
