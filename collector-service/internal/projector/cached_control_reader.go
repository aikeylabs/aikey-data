package projector

import (
	"context"
	"time"

	"github.com/AiKeyLabs/pkg/aikeytime"
	lru "github.com/hashicorp/golang-lru/v2"
)

// cachedControlEventReader wraps a ControlEventReader with an in-memory
// LRU + TTL cache keyed by virtual_key_id.
//
// # Why
//
// The projector worker pulls events from ODS in batches of `batchSize`
// (default 100). For each event it calls
// `ControlEventReader.FindByVirtualKeyAtTime` → 1 SQL roundtrip to
// `managed_key_control_events`. In production traffic, a single active
// CLI/agent emits a tight stream of events all sharing the SAME
// virtual_key_id — so a 100-event batch can fire 100 SQL calls all
// resolving to the SAME row. At ~1-5ms per PG roundtrip that's
// 100-500ms of projector wall-clock spent re-fetching identical state.
//
// This cache collapses those duplicates: only the FIRST event per
// (vk_id, cached-window) triggers a SQL call; the rest are served from
// memory in O(1).
//
// # Correctness
//
// Cache value is the single ControlEvent the inner reader returned for
// that (vk_id, eventTime) — together with its `effective_from /
// effective_to` range. A subsequent lookup for the same vk_id is a
// cache hit ONLY when both:
//
//   - the TTL hasn't expired AND
//   - the cached event's range covers the new eventTime
//
// If either fails, the wrapper falls through to the inner reader (SQL),
// then overwrites the cache with the fresh result. This keeps the cache
// SEMANTICALLY EQUIVALENT to the underlying SQL — never returns a
// control event that wouldn't have been returned by the inner reader
// for the same (vk_id, eventTime) pair.
//
// Negative results (no control event for a VK at a time, i.e. the
// inner reader returned nil, nil) are also cached so a batch full of
// orphaned events doesn't N-times retry the same miss.
//
// # Invalidation
//
// Per-entry TTL. New control events for an already-cached VK won't be
// visible until the TTL expires. The projector lags real time by a few
// seconds (FetchPending picks up rows with dwd_status='pending'); a 5-
// minute TTL is more than sufficient for control changes to land in DB
// before the events they govern start projecting. Operators who need
// stronger freshness can shrink the TTL or restart the worker (cache
// dies on process restart).
//
// # Concurrency
//
// hashicorp/golang-lru's Cache is safe for concurrent use. The
// Get-miss-then-Add window is unsynchronized: two concurrent misses
// for the same vk_id will both fire one SQL call apiece (second Add
// wins, the first's result is discarded). This is a minor wasted
// query, not a correctness issue — both queries return the same row.
// Single-flighting can be layered on if benchmarks show this matters;
// for the projector's serial per-batch processing the window is
// essentially never hit.
type cachedControlEventReader struct {
	inner ControlEventReader
	cache *lru.Cache[string, *cacheEntry]
	ttl   time.Duration
	now   func() time.Time // injected for tests; defaults to time.Now
}

type cacheEntry struct {
	// event is the ControlEvent the inner reader returned. May be nil
	// to cache a negative result (no control event found for this VK
	// at the queried time).
	event    *ControlEvent
	cachedAt time.Time
}

// NewCachedControlEventReader wraps `inner` with an LRU + TTL cache.
//
//   - size: max distinct virtual_key_ids held in the cache. The cache
//     evicts least-recently-used entries when size is exceeded. Pick a
//     number comfortably above the active-VK working set (a few
//     thousand for a busy org is plenty).
//   - ttl:  max age of a cached entry. A control event change for an
//     already-cached VK becomes visible after at most this duration.
//
// Returns the wrapper as a plain ControlEventReader so it's a drop-in
// replacement at the Enricher's call site.
func NewCachedControlEventReader(inner ControlEventReader, size int, ttl time.Duration) (ControlEventReader, error) {
	c, err := lru.New[string, *cacheEntry](size)
	if err != nil {
		return nil, err
	}
	return &cachedControlEventReader{
		inner: inner,
		cache: c,
		ttl:   ttl,
		now:   time.Now,
	}, nil
}

// FindByVirtualKeyAtTime serves from the LRU when the cached event
// still covers `eventTime` within TTL; otherwise queries the inner
// reader and re-populates.
func (r *cachedControlEventReader) FindByVirtualKeyAtTime(ctx context.Context, virtualKeyID string, eventTime aikeytime.Millis) (*ControlEvent, error) {
	now := r.now()

	if entry, ok := r.cache.Get(virtualKeyID); ok {
		// Fresh enough?
		if now.Sub(entry.cachedAt) < r.ttl {
			// Negative cache hit — still no control event for this VK.
			if entry.event == nil {
				return nil, nil
			}
			// Positive hit only if the cached event's range covers
			// THIS eventTime; otherwise the cached row would be wrong
			// for this lookup and we must fetch fresh.
			if eventTimeCovered(entry.event, eventTime) {
				return entry.event, nil
			}
		}
		// TTL expired OR range mismatch → fall through to inner.
	}

	ce, err := r.inner.FindByVirtualKeyAtTime(ctx, virtualKeyID, eventTime)
	if err != nil {
		// Errors are NOT cached. The next call retries.
		return nil, err
	}

	r.cache.Add(virtualKeyID, &cacheEntry{event: ce, cachedAt: now})
	return ce, nil
}

// eventTimeCovered reports whether `ce` is in effect at `eventTime`,
// using the half-open interval `[effective_from, effective_to)`.
// effective_to == nil means open-ended (still in effect).
func eventTimeCovered(ce *ControlEvent, eventTime aikeytime.Millis) bool {
	if eventTime < ce.EffectiveFrom {
		return false
	}
	if ce.EffectiveTo != nil && eventTime >= *ce.EffectiveTo {
		return false
	}
	return true
}
