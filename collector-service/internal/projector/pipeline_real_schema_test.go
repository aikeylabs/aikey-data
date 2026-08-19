package projector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	// Side-effect: registers ComponentControl migrations (quota tables) for the
	// quota-equivalence fence.
	_ "github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions_master"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/quota"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	_ "modernc.org/sqlite"
)

// Pre-refactor fences for the batch-transaction rewrite of the DWD projector
// (P0-4, update/20260819-审计流水线容量-P0-4核证与批量投影方案.md S1).
//
// Coverage audit 2026-08-19 found the scanOnce loop had ZERO direct tests
// (only projectOne was covered, always through mocks) and the projector's SQL
// layer only ever ran against inline simplified tables. These fences run the
// REAL loop against the REAL migrated schema, seeded through the REAL
// ingest.Service — so the batched implementation must reproduce, on production
// DDL:
//
//   1. scanOnce drains pending rows, writes DWD, and advances the checkpoint;
//   2. client_ok == ODS == DWD conservation (rows AND token sums) across
//      multi-batch drains — the in-process detector for partial-commit bugs;
//   3. the retry → dead-letter threshold boundary (19th failure retries,
//      20th dead-letters);
//   4. quota_counter equals SUM(usage_fact_dwd) after a multi-fact batch
//      (the equivalence a per-(seat,period) debounce must preserve).

// newPipelineDB boots the real schema. withControl adds ComponentControl
// (quota_subject/quota_counter) for the quota fence.
func newPipelineDB(t *testing.T, withControl bool) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	comps := []dbmigrate.Component{dbmigrate.ComponentData}
	if withControl {
		comps = []dbmigrate.Component{dbmigrate.ComponentControl, dbmigrate.ComponentData}
	}
	if err := versions.UpgradeComponentsTo(context.Background(), raw,
		dbmigrate.DialectSQLite, comps, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

// newPipelineWorker wires a worker exactly as cmd/main.go does, minus the
// cache/gap-scanner extras that are irrelevant here. The control reader is the
// real SQL one; seeded events use vault-origin VKs (personal:) so enrichment
// short-circuits deterministically without control events.
func newPipelineWorker(t *testing.T, db *shared.DB) *Worker {
	t.Helper()
	resolver, err := pricing.Load()
	if err != nil {
		t.Fatalf("load pricing: %v", err)
	}
	enricher := NewEnricher(NewSQLControlEventReader(db), resolver)
	w := NewWorker(NewSQLODSReader(db), NewSQLDWDWriter(db), NewSQLCheckpointStore(db), enricher)
	// Production wiring (cmd/main.go + appkit): batch-transaction scan path on.
	w.SetBatchDB(db)
	return w
}

// TestScanOnce_BatchTxFailureFallsBackToPerEvent: an infra SQL failure inside
// the batch transaction (deterministic RAISE(ABORT) trigger on one org's DWD
// insert) must roll the whole attempt back and replay per-event — the poisoned
// record gets its individual retry classification, every other record still
// projects, and conservation holds for the healthy org.
func TestScanOnce_BatchTxFailureFallsBackToPerEvent(t *testing.T) {
	db := newPipelineDB(t, false)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	w := newPipelineWorker(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER dwd_abort_sentinel BEFORE INSERT ON usage_fact_dwd
		 WHEN NEW.org_id = 'orgAbort'
		 BEGIN SELECT RAISE(ABORT, 'forced dwd insert failure'); END;`); err != nil {
		t.Fatalf("install abort trigger: %v", err)
	}

	seedUsageEvent(t, svc, "orgOK", "seatOK", "fb-e1", 1, 100, 50)
	seedUsageEvent(t, svc, "orgAbort", "seatAbort", "fb-poisoned", 1, 10, 5)
	seedUsageEvent(t, svc, "orgOK", "seatOK", "fb-e2", 2, 100, 50)

	w.scanOnce(ctx)

	var okDWD int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_fact_dwd WHERE org_id='orgOK'").Scan(&okDWD)
	if okDWD != 2 {
		t.Fatalf("healthy org DWD=%d want 2 (fallback must not lose them)", okDWD)
	}
	var status string
	db.QueryRowContext(ctx, "SELECT dwd_status FROM usage_event_ods WHERE event_id='fb-poisoned'").Scan(&status)
	if status != "retry" {
		t.Fatalf("poisoned record dwd_status=%s want retry (per-event replay classification)", status)
	}
	var projected int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_event_ods WHERE dwd_status='projected'").Scan(&projected)
	if projected != 2 {
		t.Fatalf("projected=%d want 2", projected)
	}
}

// seedUsageEvent pushes one event through the REAL ingest.Service (not direct
// SQL) so the fence covers the same rows production writes. Returns the event
// so a re-delivery test can resend it VERBATIM — the wire contract requires a
// re-delivered event_id to carry the SAME event_time (the proxy resends the
// persisted event, it never re-stamps; see ingest/repository_sql.go CONTRACT).
func seedUsageEvent(t *testing.T, svc *ingest.Service, org, seat, eventID string, seq, in, out int64) ingest.UsageEvent {
	t.Helper()
	now := aikeytime.Now()
	s := seq
	total := in + out
	e := ingest.UsageEvent{
		EventID: eventID, OrgID: org, SeatID: seat,
		SourceID: "srcP", SourceSeq: &s,
		EventTime: now, OccurredAt: now,
		RequestStatus: "success", RequestCount: 1,
		VirtualKeyID: "personal:vk-pipe", Model: "claude-sonnet-4-20250514", ProviderCode: "anthropic",
		InputTokens: &in, OutputTokens: &out, TotalTokens: &total,
	}
	resp, _ := svc.IngestBatch(context.Background(), &ingest.BatchRequest{Events: []ingest.UsageEvent{e}})
	if resp.Accepted != 1 {
		t.Fatalf("seed %s: accepted=%d want 1 (resp=%+v)", eventID, resp.Accepted, *resp)
	}
	return e
}

func drain(t *testing.T, w *Worker, db *shared.DB, maxRounds int) {
	t.Helper()
	for i := 0; i < maxRounds; i++ {
		w.scanOnce(context.Background())
		var pending int
		if err := db.QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM usage_event_ods WHERE dwd_status = 'pending'").Scan(&pending); err != nil {
			t.Fatalf("count pending: %v", err)
		}
		if pending == 0 {
			return
		}
	}
	t.Fatalf("projector did not drain within %d rounds", maxRounds)
}

func TestScanOnce_RealSchema_DrainsAndCheckpoints(t *testing.T) {
	db := newPipelineDB(t, false)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	w := newPipelineWorker(t, db)
	ctx := context.Background()

	const n = 7
	for i := 1; i <= n; i++ {
		seedUsageEvent(t, svc, "orgP1", "seatP1", fmt.Sprintf("p1-e%d", i), int64(i), 100, 50)
	}

	w.scanOnce(ctx)

	var projected, dwdRows int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_event_ods WHERE dwd_status='projected'").Scan(&projected)
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_fact_dwd WHERE org_id='orgP1'").Scan(&dwdRows)
	if projected != n || dwdRows != n {
		t.Fatalf("projected=%d dwd=%d want %d/%d", projected, dwdRows, n, n)
	}

	var maxOdsID int64
	db.QueryRowContext(ctx, "SELECT MAX(ods_id) FROM usage_event_ods").Scan(&maxOdsID)
	cp, err := NewSQLCheckpointStore(db).GetLastScannedOdsID(ctx, defaultTaskName)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	if cp != maxOdsID {
		t.Fatalf("checkpoint=%d want %d (max ods_id)", cp, maxOdsID)
	}
}

// TestPipeline_Conservation_ClientOkEqualsODSEqualsDWD is the in-process
// conservation detector: what the client was told landed (accepted) must equal
// the accepted ODS rows AND the DWD facts — by count and by token sum — after
// a multi-round drain (batchSize forced below N so checkpointing across rounds
// is exercised). A batched projector that partially commits, double-projects,
// or drops rows turns this red.
func TestPipeline_Conservation_ClientOkEqualsODSEqualsDWD(t *testing.T) {
	db := newPipelineDB(t, false)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	w := newPipelineWorker(t, db)
	w.batchSize = 4 // force multiple scan rounds over the 10 events
	ctx := context.Background()

	clientOk := 0
	var clientTokens int64
	seeded := make(map[int]ingest.UsageEvent)
	for i := 1; i <= 10; i++ {
		in, out := int64(100+i), int64(50+i)
		seeded[i] = seedUsageEvent(t, svc, "orgP2", "seatP2", fmt.Sprintf("p2-e%d", i), int64(i), in, out)
		clientOk++
		clientTokens += in + out
	}
	// Re-deliver two events VERBATIM (duplicates — already accounted, must not
	// inflate; same event_time per the re-delivery contract).
	for _, i := range []int{3, 7} {
		resp, _ := svc.IngestBatch(ctx, &ingest.BatchRequest{Events: []ingest.UsageEvent{seeded[i]}})
		if resp.Duplicated != 1 {
			t.Fatalf("re-delivery of p2-e%d: duplicated=%d want 1", i, resp.Duplicated)
		}
	}

	drain(t, w, db, 10)

	var odsRows, dwdRows int
	var odsTokens, dwdTokens sql.NullInt64
	db.QueryRowContext(ctx,
		"SELECT COUNT(*), SUM(input_tokens + output_tokens) FROM usage_event_ods WHERE org_id='orgP2' AND ingest_status='accepted'").
		Scan(&odsRows, &odsTokens)
	db.QueryRowContext(ctx,
		"SELECT COUNT(*), SUM(input_tokens + output_tokens) FROM usage_fact_dwd WHERE org_id='orgP2'").
		Scan(&dwdRows, &dwdTokens)

	if odsRows != clientOk || dwdRows != clientOk {
		t.Fatalf("conservation broken: client_ok=%d ODS=%d DWD=%d", clientOk, odsRows, dwdRows)
	}
	if odsTokens.Int64 != clientTokens || dwdTokens.Int64 != clientTokens {
		t.Fatalf("token conservation broken: client=%d ODS=%d DWD=%d",
			clientTokens, odsTokens.Int64, dwdTokens.Int64)
	}

	var projected int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_event_ods WHERE dwd_status='projected'").Scan(&projected)
	if projected != clientOk {
		t.Fatalf("projected=%d want %d", projected, clientOk)
	}
}

// TestBacklogSnapshot_RedWhenStalledGreenWhenDrained is the 能红 fence for the
// P0-4 backlog gauges: with the projector stalled the gauges MUST show the
// backlog (depth + a positive oldest age); after draining they return to zero.
// A worker without a batch DB reports Available=false (never a fake-healthy
// empty backlog).
func TestBacklogSnapshot_RedWhenStalledGreenWhenDrained(t *testing.T) {
	db := newPipelineDB(t, false)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	w := newPipelineWorker(t, db)
	ctx := context.Background()

	for i := 1; i <= 4; i++ {
		seedUsageEvent(t, svc, "orgBL", "seatBL", fmt.Sprintf("bl-e%d", i), int64(i), 10, 5)
	}

	// Stalled (projector not yet run) → backlog visible.
	snap := w.Backlog(ctx)
	if !snap.Available {
		t.Fatalf("backlog must be available with a batch DB wired")
	}
	if snap.Pending != 4 {
		t.Fatalf("stalled backlog pending=%d want 4", snap.Pending)
	}
	if snap.OldestPendingAgeMS < 0 {
		t.Fatalf("oldest age must be non-negative, got %d", snap.OldestPendingAgeMS)
	}

	drain(t, w, db, 5)

	snap = w.Backlog(ctx)
	if snap.Pending != 0 || snap.OldestPendingAgeMS != 0 {
		t.Fatalf("drained backlog=%+v want zeros", snap)
	}

	// No batch DB → gauges explicitly unavailable, not fake-zero-healthy.
	bare := NewWorker(w.odsReader, w.dwdWriter, w.checkpoint, w.enricher)
	if got := bare.Backlog(ctx); got.Available {
		t.Fatalf("worker without batch DB must report Available=false, got %+v", got)
	}
}

// erroringControlReader fails every managed-VK lookup, deterministically
// forcing the Enrich error path (vault-origin VKs never reach it).
type erroringControlReader struct{}

func (erroringControlReader) FindByVirtualKeyAtTime(context.Context, string, aikeytime.Millis) (*ControlEvent, error) {
	return nil, errors.New("forced control lookup failure")
}

// TestScanOnce_RetryThenDeadLetterBoundary pins handleError's threshold: the
// 19th consecutive failure still schedules a retry; the 20th dead-letters.
// The batched rewrite moves these UPDATEs into a transaction — the boundary
// must not drift.
func TestScanOnce_RetryThenDeadLetterBoundary(t *testing.T) {
	db := newPipelineDB(t, false)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	resolver, err := pricing.Load()
	if err != nil {
		t.Fatalf("load pricing: %v", err)
	}
	w := NewWorker(NewSQLODSReader(db), NewSQLDWDWriter(db), NewSQLCheckpointStore(db),
		NewEnricher(erroringControlReader{}, resolver))
	ctx := context.Background()

	now := aikeytime.Now()
	in, out := int64(10), int64(5)
	total := in + out
	seq := int64(1)
	e := ingest.UsageEvent{
		EventID: "dl-e1", OrgID: "orgDL", SeatID: "seatDL",
		SourceID: "srcDL", SourceSeq: &seq,
		EventTime: now, OccurredAt: now,
		RequestStatus: "success", RequestCount: 1,
		// Managed (non-vault) VK → control lookup → forced error.
		VirtualKeyID: "vk-managed-dl", Model: "m", ProviderCode: "p",
		InputTokens: &in, OutputTokens: &out, TotalTokens: &total,
	}
	if resp, _ := svc.IngestBatch(ctx, &ingest.BatchRequest{Events: []ingest.UsageEvent{e}}); resp.Accepted != 1 {
		t.Fatalf("seed: %+v", *resp)
	}

	// 19th failure: dwd_retry_count=18 stored, next attempt → 19 → still retry.
	if _, err := db.ExecContext(ctx,
		"UPDATE usage_event_ods SET dwd_retry_count=18, dwd_status='pending' WHERE event_id='dl-e1'"); err != nil {
		t.Fatalf("prep retry_count: %v", err)
	}
	w.scanOnce(ctx)
	var status string
	var count int
	db.QueryRowContext(ctx, "SELECT dwd_status, dwd_retry_count FROM usage_event_ods WHERE event_id='dl-e1'").Scan(&status, &count)
	if status != "retry" || count != 19 {
		t.Fatalf("after 19th failure: status=%s count=%d want retry/19", status, count)
	}

	// 20th failure: → dead_letter.
	if _, err := db.ExecContext(ctx,
		"UPDATE usage_event_ods SET dwd_status='pending' WHERE event_id='dl-e1'"); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	w.scanOnce(ctx)
	db.QueryRowContext(ctx, "SELECT dwd_status FROM usage_event_ods WHERE event_id='dl-e1'").Scan(&status)
	if status != "dead_letter" {
		t.Fatalf("after 20th failure: status=%s want dead_letter", status)
	}
	if w.MetricsSnapshot().DeadLetter != 1 {
		t.Fatalf("dead_letter metric=%d want 1", w.MetricsSnapshot().DeadLetter)
	}
}

// TestPipeline_QuotaCounterMatchesDWDSum: after projecting a multi-fact batch
// for one seat, quota_counter must equal SUM(usage_fact_dwd) exactly. The S3
// debounce (one recompute per (subject, period) per batch instead of per fact)
// must keep this equality — RecomputeFromDWD is an absolute materialized view,
// so collapsing calls cannot change the final value. This fence is the proof
// harness for that equivalence.
func TestPipeline_QuotaCounterMatchesDWDSum(t *testing.T) {
	db := newPipelineDB(t, true)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	w := newPipelineWorker(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO quota_subject (subject_id, subject_kind, display_name, members, rules,
			source_system, external_ref, enabled, created_at, updated_at)
		VALUES ('seatQ', 'seat', '', NULL,
			'[{"metric":"tokens","period":"monthly","limit_amount":100000,"thresholds":[{"pct":80,"action":"warn"}]}]',
			'aikey_seat', 'seatQ', 1, 0, 0)`); err != nil {
		t.Fatalf("insert quota subject: %v", err)
	}
	w.SetQuotaMaterializer(quota.NewMaterializer(quota.NewStorage(db), time.Hour))

	var wantTokens int64
	for i := 1; i <= 5; i++ {
		in, out := int64(1000*i), int64(500*i)
		seedUsageEvent(t, svc, "orgQ", "seatQ", fmt.Sprintf("q-e%d", i), int64(i), in, out)
		wantTokens += in + out
	}

	drain(t, w, db, 5)

	var dwdSum int64
	db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(input_tokens + output_tokens + cached_input_tokens + cache_creation_input_tokens + reasoning_tokens), 0)
		 FROM usage_fact_dwd WHERE seat_id='seatQ'`).Scan(&dwdSum)
	if dwdSum != wantTokens {
		t.Fatalf("dwd token sum=%d want %d", dwdSum, wantTokens)
	}

	var counter float64
	if err := db.QueryRowContext(ctx,
		"SELECT used_amount FROM quota_counter WHERE subject_id='seatQ' AND metric='tokens'").Scan(&counter); err != nil {
		t.Fatalf("read quota_counter: %v", err)
	}
	if int64(counter) != wantTokens {
		t.Fatalf("quota_counter=%v want %d (must equal SUM(DWD))", counter, wantTokens)
	}
}
