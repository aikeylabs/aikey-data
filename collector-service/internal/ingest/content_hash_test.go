package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	"github.com/AiKeyLabs/pkg/usagehash"
	_ "modernc.org/sqlite"
)

// newContentHashTestDB boots the REAL data-component schema (rc.7 incl. the
// stage-C content_hash column + ingest_status disposition) so these tests
// exercise the production INSERT bind list and disposition columns, not a
// hand-rolled CREATE TABLE (test-fixture-real-schema + schema-code-coherence).
func newContentHashTestDB(t *testing.T) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := versions.UpgradeComponentsTo(context.Background(), raw,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

// hashEvent builds a v2 usage event with the given metering values and returns
// it plus the CORRECT content hash for those values. total_tokens is set to
// in+out exactly as the proxy computes it.
func hashEvent(id string, seq, in, out, cacheR, cacheC int64, model, provider string) (*UsageEvent, string) {
	now := aikeytime.Now()
	total := in + out
	s := seq
	e := &UsageEvent{
		EventID: id, OrgID: "orgH", SourceID: "srcH", SourceSeq: &s,
		EventTime: now, OccurredAt: now, RequestStatus: "success", RequestCount: 1,
		Model: model, ProviderCode: provider,
		InputTokens: &in, OutputTokens: &out, TotalTokens: &total,
		CachedInputTokens: &cacheR, CacheCreationInputTokens: &cacheC,
	}
	h := usagehash.Compute(usagehash.Input{
		InputTokens: in, OutputTokens: out, TotalTokens: total,
		CacheReadInputTokens: cacheR, CacheCreationInputTokens: cacheC,
		Model: model, ProviderCode: provider,
	})
	return e, h
}

// disposition reads back the stored ingest_status / dwd_status / content_hash.
func disposition(t *testing.T, db *shared.DB, eventID string) (ingestStatus, dwdStatus string, contentHash sql.NullString) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		"SELECT ingest_status, dwd_status, content_hash FROM usage_event_ods WHERE event_id = ?",
		eventID).Scan(&ingestStatus, &dwdStatus, &contentHash)
	if err != nil {
		t.Fatalf("read disposition for %s: %v", eventID, err)
	}
	return
}

func ingestOneEvent(t *testing.T, svc *Service, e *UsageEvent) EventResult {
	t.Helper()
	resp, results := svc.IngestBatch(context.Background(), &BatchRequest{Events: []UsageEvent{*e}})
	_ = resp
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	return results[0]
}

// TestContentHash_ValidAccepted: a correct hash → accepted + billable (dwd_status
// 'pending' so the projector will pick it up).
func TestContentHash_ValidAccepted(t *testing.T) {
	db := newContentHashTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	e, h := hashEvent("h-ok", 1, 100, 50, 10, 20, "claude", "anthropic")
	e.ContentHash = h

	if r := ingestOneEvent(t, svc, e); r.Status != "accepted" {
		t.Fatalf("status=%s, want accepted", r.Status)
	}
	is, dwd, _ := disposition(t, db, "h-ok")
	if is != "accepted" || dwd != "pending" {
		t.Fatalf("disposition ingest=%s dwd=%s, want accepted/pending (billable)", is, dwd)
	}
	if got := svc.MetricsSnapshot().Quarantined; got != 0 {
		t.Fatalf("quarantined metric=%d, want 0", got)
	}
}

// TestContentHash_MismatchQuarantined is the core C guarantee: a corrupted
// metering field (output 50→0 while the hash still claims 50) is detected and
// QUARANTINED — stored for review (ingest_status/dwd_status both 'quarantined'
// so the projector never bills it), NOT silently accepted as a real 0.
func TestContentHash_MismatchQuarantined(t *testing.T) {
	db := newContentHashTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	// Hash computed for output=50, but the event actually carries output=0.
	e, h := hashEvent("h-bad", 1, 100, 50, 10, 20, "claude", "anthropic")
	e.ContentHash = h
	zero := int64(0)
	e.OutputTokens = &zero            // corruption: the silent-zero bug
	total := int64(100)               // total no longer matches in+out=100? in=100,out=0→total should be 100
	e.TotalTokens = &total            // keep total consistent with in to isolate the output corruption

	if r := ingestOneEvent(t, svc, e); r.Status != "quarantined" {
		t.Fatalf("status=%s, want quarantined", r.Status)
	}
	is, dwd, ch := disposition(t, db, "h-bad")
	if is != "quarantined" || dwd != "quarantined" {
		t.Fatalf("disposition ingest=%s dwd=%s, want quarantined/quarantined (stored, not billed)", is, dwd)
	}
	if !ch.Valid || ch.String != h {
		t.Fatalf("stored content_hash=%v, want the client-stamped %s (preserved for review)", ch, h)
	}
	if got := svc.MetricsSnapshot().Quarantined; got != 1 {
		t.Fatalf("quarantined metric=%d, want 1", got)
	}
}

// TestContentHash_AbsentAccepted: an event with no content_hash (older proxy)
// is accepted normally — validation is skipped, never quarantined (conserve).
func TestContentHash_AbsentAccepted(t *testing.T) {
	db := newContentHashTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	e, _ := hashEvent("h-absent", 1, 100, 50, 0, 0, "gpt-4o", "openai")
	e.ContentHash = "" // older proxy

	if r := ingestOneEvent(t, svc, e); r.Status != "accepted" {
		t.Fatalf("status=%s, want accepted (no hash → skip validation)", r.Status)
	}
	is, _, _ := disposition(t, db, "h-absent")
	if is != "accepted" {
		t.Fatalf("ingest_status=%s, want accepted", is)
	}
}

// TestContentHash_UnknownSchemeAccepted: a hash from a future/unknown scheme is
// skipped (accepted), never false-quarantined — the forward-compat guarantee
// that lets a newer client reach an older collector safely.
func TestContentHash_UnknownSchemeAccepted(t *testing.T) {
	db := newContentHashTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	e, _ := hashEvent("h-future", 1, 100, 50, 10, 20, "claude", "anthropic")
	e.ContentHash = "sha256:2:deadbeefdeadbeef" // scheme 2, unknown to this build

	if r := ingestOneEvent(t, svc, e); r.Status != "accepted" {
		t.Fatalf("status=%s, want accepted (unknown scheme → skip, not quarantine)", r.Status)
	}
	if got := svc.MetricsSnapshot().Quarantined; got != 0 {
		t.Fatalf("quarantined metric=%d, want 0 (must not false-quarantine unknown scheme)", got)
	}
}

// TestContentHash_ConflictOnDuplicate (§6.2): the same event_id re-delivered
// with a DIFFERENT hash is a corruption/tamper conflict. The first-stored row is
// kept (its hash unchanged) and the conflict is counted — never silently
// overwritten.
func TestContentHash_ConflictOnDuplicate(t *testing.T) {
	db := newContentHashTestDB(t)
	svc := NewService(NewSQLODSRepository(db))

	e1, h1 := hashEvent("h-dup", 1, 100, 50, 10, 20, "claude", "anthropic")
	e1.ContentHash = h1
	if r := ingestOneEvent(t, svc, e1); r.Status != "accepted" {
		t.Fatalf("first insert status=%s, want accepted", r.Status)
	}

	// Same event_id, different metering → different hash. A genuine
	// re-delivery carries the SAME event_time (the proxy stamps it once and
	// resends the persisted event verbatim — see InsertEvent's dedup
	// contract), so we pin e2's event_time to e1's. Without this, two
	// hashEvent() calls take aikeytime.Now() a hair apart and, when they
	// straddle a millisecond boundary, the (org_id,event_id,event_time)
	// verify misses the stored row and the duplicate is misread as rejected
	// (flaky). See workflow/CI/bugfix/2026-06-10-collector-dedup-event-time-misclassify.md.
	e2, h2 := hashEvent("h-dup", 1, 100, 999, 10, 20, "claude", "anthropic")
	e2.EventTime = e1.EventTime
	e2.OccurredAt = e1.OccurredAt
	e2.ContentHash = h2
	if h1 == h2 {
		t.Fatal("test setup: the two hashes should differ")
	}
	r := ingestOneEvent(t, svc, e2)
	if r.Status != "duplicated" {
		t.Fatalf("second insert status=%s, want duplicated", r.Status)
	}
	if got := svc.MetricsSnapshot().Conflicts; got != 1 {
		t.Fatalf("conflicts metric=%d, want 1 (hash mismatch on duplicate)", got)
	}
	// The first row's hash must be preserved (not overwritten by the conflicting retransmit).
	_, _, ch := disposition(t, db, "h-dup")
	if !ch.Valid || ch.String != h1 {
		t.Fatalf("stored hash=%v, want the FIRST %s (never silently pick the second)", ch, h1)
	}
}

// 🔴 The two upstream-fallback fields must be DECLARED on UsageEvent, not merely
// present on the wire (openspec `aliyun-aigw-p0-upstream-fallback`, task 3.7).
//
// raw_event_json is a re-marshal of the UsageEvent struct, NOT the verbatim wire
// bytes — so a field the struct does not declare is dropped before anything
// touches disk. The proxy has been sending both since P3; until they were
// declared they were being discarded here, silently, which is the difference
// between "the collector has not projected them yet" and "the data never
// existed". A wire round-trip is the only check that can tell those apart.
func TestUsageEventKeepsFallbackAttributionThroughAWireRoundTrip(t *testing.T) {
	const wire = `{"event_id":"e1","org_id":"o1","fallback_reason":"UPSTREAM_5XX","fallback_attempt":2}`

	var e UsageEvent
	if err := json.Unmarshal([]byte(wire), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.FallbackReason != "UPSTREAM_5XX" {
		t.Errorf("FallbackReason = %q, want UPSTREAM_5XX — an undeclared field is dropped here", e.FallbackReason)
	}
	if e.FallbackAttempt == nil || *e.FallbackAttempt != 2 {
		t.Fatalf("FallbackAttempt = %v, want 2", e.FallbackAttempt)
	}

	// And back out again, because raw_event_json is produced by marshalling this
	// struct: a field that survives Unmarshal but is not emitted is still lost.
	out, err := json.Marshal(&e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"fallback_reason":"UPSTREAM_5XX"`, `"fallback_attempt":2`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("re-marshalled event is missing %s\ngot: %s", want, out)
		}
	}

	// 🔴 An event with no attempt number must emit NOTHING for it, so the column
	// receives NULL. Emitting 0 or 1 would turn "the sender said nothing" into a
	// measurement, and every count over that column would then be wrong in a way
	// no query could detect.
	var silent UsageEvent
	if err := json.Unmarshal([]byte(`{"event_id":"e2"}`), &silent); err != nil {
		t.Fatalf("unmarshal silent: %v", err)
	}
	if silent.FallbackAttempt != nil {
		t.Errorf("FallbackAttempt = %v for an event that carried none; want nil", silent.FallbackAttempt)
	}
	quiet, _ := json.Marshal(&silent)
	if strings.Contains(string(quiet), "fallback_attempt") {
		t.Errorf("an event with no attempt number still emits the field: %s", quiet)
	}
}
