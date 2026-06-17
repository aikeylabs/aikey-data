package conversation

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

const (
	// defaultKnownLossScanInterval is how often the worker scans for aged gaps.
	defaultKnownLossScanInterval = 5 * time.Minute
	// defaultKnownLossTimeout is how long a gap must persist before a still-missing
	// seq is treated as lost. Generous vs the content WAL's retry/backoff window
	// (5s tick + exponential backoff capped at 5min) so a survivor is genuinely
	// gone, not merely in-flight.
	defaultKnownLossTimeout = 30 * time.Minute
)

// KnownLossWorker periodically promotes aged, still-missing conversation
// source_seq gaps to conversation_known_loss_ledger and re-advances the
// watermark. Without it a permanently-lost seq (e.g. a content-WAL drop on the
// proxy) would stall contiguous_seq — and therefore the proxy's content-WAL
// pruning — forever. Self-contained on the conversation_* tables; mirrors usage's
// projector stale-gap promotion but never touches the usage path.
type KnownLossWorker struct {
	repo     Repository
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger
}

// KnownLossOption tunes the worker (tests shrink the timings).
type KnownLossOption func(*KnownLossWorker)

func WithKnownLossInterval(d time.Duration) KnownLossOption {
	return func(w *KnownLossWorker) {
		if d > 0 {
			w.interval = d
		}
	}
}

func WithKnownLossTimeout(d time.Duration) KnownLossOption {
	return func(w *KnownLossWorker) {
		if d > 0 {
			w.timeout = d
		}
	}
}

func NewKnownLossWorker(repo Repository, logger *slog.Logger, opts ...KnownLossOption) *KnownLossWorker {
	if logger == nil {
		logger = slog.Default()
	}
	w := &KnownLossWorker{
		repo:     repo,
		interval: defaultKnownLossScanInterval,
		timeout:  defaultKnownLossTimeout,
		logger:   logger,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Run blocks until ctx is cancelled, promoting stale gaps each tick.
func (w *KnownLossWorker) Run(ctx context.Context) {
	w.logger.Info("conversation known-loss worker started",
		"event.name", "conversation.known_loss.worker_started",
		"interval", w.interval.String(), "timeout", w.timeout.String())
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.PromoteOnce(ctx)
		}
	}
}

// PromoteOnce scans once for aged gaps and promotes each. Exposed for tests and
// on-demand reconcile. Panic-isolated: a background-worker panic must never take
// down the collector's ingest path.
func (w *KnownLossWorker) PromoteOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("conversation known-loss: panic recovered",
				"event.name", "conversation.known_loss.panic",
				"error.code", "CONTENT_KNOWN_LOSS_PANIC", "panic", r, "stack", string(debug.Stack()))
		}
	}()

	staleBefore := int64(aikeytime.Now()) - w.timeout.Milliseconds()
	gaps, err := w.repo.ScanStaleGaps(ctx, staleBefore)
	if err != nil {
		w.logger.Warn("conversation known-loss: scan failed",
			"event.name", "conversation.known_loss.scan_failed", "error", err)
		return
	}
	for i := range gaps {
		g := gaps[i]
		missing, err := w.repo.EnumerateMissingSeqs(ctx, g.OrgID, g.SourceID, g.Contiguous, g.HighWater())
		if err != nil {
			w.logger.Warn("conversation known-loss: enumerate failed",
				"event.name", "conversation.known_loss.enumerate_failed",
				"org_id", g.OrgID, "source_id", g.SourceID, "error", err)
			continue
		}
		if len(missing) == 0 {
			continue // gap closed between scan and enumerate, or already ledgered
		}
		contiguous, err := w.repo.RecordKnownLoss(ctx, g.OrgID, g.SourceID, missing, "stale_gap")
		if err != nil {
			w.logger.Error("conversation known-loss: record failed",
				"event.name", "conversation.known_loss.record_failed",
				"error.code", "CONTENT_KNOWN_LOSS_RECORD_FAILED",
				"org_id", g.OrgID, "source_id", g.SourceID, "error", err)
			continue
		}
		w.logger.Warn("conversation known-loss: seqs promoted, watermark advanced",
			"event.name", "conversation.known_loss.promoted",
			"org_id", g.OrgID, "source_id", g.SourceID,
			"promoted_count", len(missing), "first_seq", missing[0], "last_seq", missing[len(missing)-1],
			"contiguous_seq", contiguous)
	}
}
