package projector

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// fakeReader records call counts + returns scripted results.
type fakeReader struct {
	calls atomic.Int64
	// per-call return values; index by call sequence
	results []fakeResult
}

type fakeResult struct {
	ce  *ControlEvent
	err error
}

func (f *fakeReader) FindByVirtualKeyAtTime(_ context.Context, _ string, _ aikeytime.Millis) (*ControlEvent, error) {
	idx := f.calls.Add(1) - 1
	if int(idx) >= len(f.results) {
		// default: return last result for any overflow calls
		idx = int64(len(f.results)) - 1
	}
	return f.results[idx].ce, f.results[idx].err
}

func mkCE(vk string, from, to int64) *ControlEvent {
	ce := &ControlEvent{VirtualKeyID: vk, EffectiveFrom: aikeytime.Millis(from)}
	if to != 0 {
		t := aikeytime.Millis(to)
		ce.EffectiveTo = &t
	}
	return ce
}

func newCached(t *testing.T, inner ControlEventReader, ttl time.Duration) *cachedControlEventReader {
	t.Helper()
	r, err := NewCachedControlEventReader(inner, 100, ttl)
	if err != nil {
		t.Fatalf("NewCachedControlEventReader: %v", err)
	}
	return r.(*cachedControlEventReader)
}

func TestCache_HitWithinTTLAndRange(t *testing.T) {
	// 1 control event for vk-A covering [100, ∞). 5 events at times in
	// that range → exactly 1 SQL call, 4 cache hits.
	inner := &fakeReader{results: []fakeResult{{ce: mkCE("vk-A", 100, 0)}}}
	r := newCached(t, inner, 5*time.Minute)

	ctx := context.Background()
	for _, ts := range []int64{120, 200, 500, 1000, 2000} {
		ce, err := r.FindByVirtualKeyAtTime(ctx, "vk-A", aikeytime.Millis(ts))
		if err != nil {
			t.Fatalf("lookup at %d: %v", ts, err)
		}
		if ce == nil || ce.VirtualKeyID != "vk-A" {
			t.Fatalf("expected vk-A control event at %d, got %v", ts, ce)
		}
	}
	if got := inner.calls.Load(); got != 1 {
		t.Errorf("expected 1 SQL call (1 prime + 4 hits), got %d", got)
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	// Same call after TTL expiry must re-prime from inner.
	inner := &fakeReader{results: []fakeResult{
		{ce: mkCE("vk-A", 100, 0)},
		{ce: mkCE("vk-A", 100, 0)},
	}}
	r := newCached(t, inner, 50*time.Millisecond)
	frozen := time.Unix(0, 0)
	r.now = func() time.Time { return frozen }

	ctx := context.Background()
	_, _ = r.FindByVirtualKeyAtTime(ctx, "vk-A", 150) // prime
	_, _ = r.FindByVirtualKeyAtTime(ctx, "vk-A", 150) // hit (TTL not advanced)

	// Advance "now" past TTL.
	frozen = frozen.Add(60 * time.Millisecond)
	_, _ = r.FindByVirtualKeyAtTime(ctx, "vk-A", 150) // re-prime

	if got := inner.calls.Load(); got != 2 {
		t.Errorf("expected 2 SQL calls (initial + re-prime), got %d", got)
	}
}

func TestCache_RangeMismatch_FallsThroughToSQL(t *testing.T) {
	// Cached event covers [200, 300). Lookups at 250 hit; lookup at
	// 100 OR 500 must fall through to inner because the cached event
	// doesn't cover those times — keeping the cache semantically
	// identical to direct SQL.
	inner := &fakeReader{results: []fakeResult{
		{ce: mkCE("vk-A", 200, 300)},
		{ce: mkCE("vk-A", 50, 0)}, // refreshed answer for the 100-time lookup
	}}
	r := newCached(t, inner, 5*time.Minute)

	ctx := context.Background()
	_, _ = r.FindByVirtualKeyAtTime(ctx, "vk-A", 250) // prime [200,300)
	_, _ = r.FindByVirtualKeyAtTime(ctx, "vk-A", 250) // hit
	_, _ = r.FindByVirtualKeyAtTime(ctx, "vk-A", 100) // miss (eventTime < from) → SQL

	if got := inner.calls.Load(); got != 2 {
		t.Errorf("expected 2 SQL calls (prime + range-miss reprime), got %d", got)
	}
}

func TestCache_NegativeResultCached(t *testing.T) {
	// Inner returns (nil, nil) — no control event for this VK. The
	// next call within TTL must NOT re-hit inner; that's a "negative
	// cache" hit.
	inner := &fakeReader{results: []fakeResult{{ce: nil}}}
	r := newCached(t, inner, 5*time.Minute)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		ce, err := r.FindByVirtualKeyAtTime(ctx, "vk-orphan", 1000)
		if err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
		if ce != nil {
			t.Fatalf("expected nil for orphaned VK, got %v", ce)
		}
	}
	if got := inner.calls.Load(); got != 1 {
		t.Errorf("expected 1 SQL call (prime + 2 negative-cache hits), got %d", got)
	}
}

func TestCache_DifferentVKsIndependent(t *testing.T) {
	// Each distinct VK primes the cache independently. 2 VKs × 5
	// events each = 2 SQL calls, 8 hits.
	inner := &fakeReader{results: []fakeResult{
		{ce: mkCE("vk-A", 100, 0)},
		{ce: mkCE("vk-B", 200, 0)},
	}}
	r := newCached(t, inner, 5*time.Minute)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = r.FindByVirtualKeyAtTime(ctx, "vk-A", aikeytime.Millis(150+int64(i)))
		_, _ = r.FindByVirtualKeyAtTime(ctx, "vk-B", aikeytime.Millis(250+int64(i)))
	}
	if got := inner.calls.Load(); got != 2 {
		t.Errorf("expected 2 SQL calls (one per VK), got %d", got)
	}
}

func TestCache_ErrorsAreNotCached(t *testing.T) {
	// inner returns error → wrapper propagates and does NOT cache.
	// Next call must hit inner again (no false silent success).
	failures := &fakeReader{results: []fakeResult{
		{err: context.Canceled},
		{ce: mkCE("vk-A", 100, 0)},
	}}
	r := newCached(t, failures, 5*time.Minute)

	ctx := context.Background()
	if _, err := r.FindByVirtualKeyAtTime(ctx, "vk-A", 150); err == nil {
		t.Fatal("expected error on first call")
	}
	if _, err := r.FindByVirtualKeyAtTime(ctx, "vk-A", 150); err != nil {
		t.Fatalf("expected success on second call, got %v", err)
	}
	if got := failures.calls.Load(); got != 2 {
		t.Errorf("error should not have been cached: expected 2 calls, got %d", got)
	}
}

func TestEventTimeCovered_Boundaries(t *testing.T) {
	ce := mkCE("vk-A", 100, 200) // [100, 200)
	cases := []struct {
		ts   int64
		want bool
	}{
		{99, false},   // before
		{100, true},   // inclusive lower
		{150, true},   // middle
		{199, true},   // inside upper boundary
		{200, false},  // exclusive upper
		{500, false},  // after
	}
	for _, c := range cases {
		if got := eventTimeCovered(ce, aikeytime.Millis(c.ts)); got != c.want {
			t.Errorf("eventTimeCovered(ts=%d) = %v, want %v", c.ts, got, c.want)
		}
	}

	// Open-ended (effective_to nil) → always true for ts >= from.
	open := mkCE("vk-A", 100, 0)
	if !eventTimeCovered(open, 100) {
		t.Error("open-ended: expected coverage at 100")
	}
	if !eventTimeCovered(open, 1_000_000_000_000) {
		t.Error("open-ended: expected coverage far in future")
	}
	if eventTimeCovered(open, 50) {
		t.Error("open-ended: should NOT cover ts < from")
	}
}
