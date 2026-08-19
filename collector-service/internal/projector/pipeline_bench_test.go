package projector

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	_ "modernc.org/sqlite"
)

// S2 baseline benchmarks for the batch-transaction rewrite (P0-4,
// update/20260819-审计流水线容量-P0-4核证与批量投影方案.md). Run BEFORE and
// AFTER S3 and compare:
//
//	go test -bench 'BenchmarkPipeline' -benchtime 5x -run '^$' ./internal/projector/ | tee baseline.txt
//
// Why FILE-backed SQLite, not :memory:: the dominant cost being removed is the
// per-event COMMIT → fsync (PostgreSQL synchronous_commit=on has the same
// per-commit WAL fsync shape). An in-memory DB has no fsync at all and would
// under-measure the win by an order of magnitude. File-backed SQLite with its
// default synchronous=FULL journal is the closest in-process stand-in; the
// authoritative PostgreSQL numbers come from the capacity ladder on release
// hardware (S7) — these benches are for before/after comparison only, not a
// capacity claim.

const benchBatch = 500 // wire maximum per HTTP batch (api/ingest.go maxBatchSize)

func newBenchDB(b *testing.B) *shared.DB {
	b.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("open sqlite: %v", err)
	}
	b.Cleanup(func() { raw.Close() })
	if err := versions.UpgradeComponentsTo(context.Background(), raw,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

func benchEvents(round, n int) []ingest.UsageEvent {
	now := aikeytime.Now()
	events := make([]ingest.UsageEvent, 0, n)
	for i := 0; i < n; i++ {
		seq := int64(round*n + i + 1)
		in, out := int64(100), int64(50)
		total := in + out
		events = append(events, ingest.UsageEvent{
			EventID: fmt.Sprintf("bench-r%d-e%d", round, i), OrgID: "orgBench", SeatID: "seatBench",
			SourceID: "srcBench", SourceSeq: &seq,
			EventTime: now, OccurredAt: now,
			RequestStatus: "success", RequestCount: 1,
			VirtualKeyID: "personal:vk-bench", Model: "claude-sonnet-4-20250514", ProviderCode: "anthropic",
			InputTokens: &in, OutputTokens: &out, TotalTokens: &total,
		})
	}
	return events
}

// BenchmarkPipelineIngestBatch500 measures one full wire-maximum ingest batch
// (500 events) through the real Service against the real schema. Baseline
// (per-event autocommit): 500 commits/batch. Post-S3 target: 1 commit/batch.
func BenchmarkPipelineIngestBatch500(b *testing.B) {
	db := newBenchDB(b)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, _ := svc.IngestBatch(ctx, &ingest.BatchRequest{Events: benchEvents(i, benchBatch)})
		if resp.Accepted != benchBatch {
			b.Fatalf("round %d: accepted=%d want %d", i, resp.Accepted, benchBatch)
		}
	}
	b.ReportMetric(float64(benchBatch)*float64(b.N)/b.Elapsed().Seconds(), "events/sec")
}

// BenchmarkPipelineProjectorDrain500 measures draining 500 pending ODS rows to
// DWD (5 scan rounds at the production batchSize=100). Baseline: 2 commits per
// event (DWD insert + MarkProjected) + 1 checkpoint per round ≈ 1005 commits.
// Post-S3 target: 1 commit per scan round = 5.
func BenchmarkPipelineProjectorDrain500(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db := newBenchDB(b)
		svc := ingest.NewService(ingest.NewSQLODSRepository(db))
		for r := 0; r*benchBatch < 500; r++ {
			resp, _ := svc.IngestBatch(ctx, &ingest.BatchRequest{Events: benchEvents(r, benchBatch)})
			if resp.Accepted != benchBatch {
				b.Fatalf("seed round %d: accepted=%d", r, resp.Accepted)
			}
		}
		resolver, perr := pricing.Load()
		if perr != nil {
			b.Fatalf("load pricing: %v", perr)
		}
		w := NewWorker(NewSQLODSReader(db), NewSQLDWDWriter(db), NewSQLCheckpointStore(db),
			NewEnricher(NewSQLControlEventReader(db), resolver))
		// Production wiring: batch-transaction scan path (cmd/main.go + appkit).
		w.SetBatchDB(db)
		b.StartTimer()

		for round := 0; round < 20; round++ {
			w.scanOnce(ctx)
			var pending int
			db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_event_ods WHERE dwd_status='pending'").Scan(&pending)
			if pending == 0 {
				break
			}
		}
		b.StopTimer()
		var dwd int
		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_fact_dwd").Scan(&dwd)
		if dwd != 500 {
			b.Fatalf("drain incomplete: dwd=%d want 500", dwd)
		}
		b.StartTimer()
	}
	b.ReportMetric(500*float64(b.N)/b.Elapsed().Seconds(), "events/sec")
}
