package projector

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/integrity"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	_ "modernc.org/sqlite"
)

// TestScanNow_PromotesAndReturnsSettled is the D2 reconcile core: ScanNow forces
// a scan + known-loss promotion and returns the SETTLED per-source view (a
// just-promoted stale gap shows up as known_loss with contiguous advanced, not
// as an open gap). Uses a real schema + real ingest repo as the LossPromoter.
func TestScanNow_PromotesAndReturnsSettled(t *testing.T) {
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := versions.UpgradeComponentsTo(context.Background(), raw,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db := shared.NewDB(raw, shared.DialectSQLite)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()

	// Seed a stale middle gap: seqs 1,2,3,5 (hole at 4) → contiguous=3, max_seen=5.
	for _, seq := range []int64{1, 2, 3, 5} {
		s := seq
		now := aikeytime.Now()
		e := &ingest.UsageEvent{
			EventID: "e" + string(rune('0'+seq)), OrgID: "orgS", SourceID: "srcS", SourceSeq: &s,
			EventTime: now, OccurredAt: now, RequestStatus: "success", RequestCount: 1,
		}
		if _, _, err := repo.InsertEvent(ctx, e, []byte("{}"), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.AdvanceWatermark(ctx, "orgS", "srcS", 0); err != nil {
		t.Fatal(err)
	}
	// Age the gap past KnownLossTimeout so it's Promotable.
	if _, err := db.ExecContext(ctx,
		"UPDATE usage_source_watermark SET updated_at=(CAST(strftime('%s','now') AS INTEGER)*1000 - ?) WHERE source_id='srcS'",
		(25 * time.Hour).Milliseconds()); err != nil {
		t.Fatal(err)
	}

	// Worker with real scanner + promoter (projection deps unused by ScanNow).
	w := NewWorker(&mockODSReader{}, &mockDWDWriter{}, &mockCheckpointStore{}, NewEnricher(&mockControlReader{}, nil))
	w.SetGapScanner(integrity.NewScanner(db, integrity.DefaultCriteria()))
	w.SetLossPromoter(repo)

	findings, err := w.ScanNow(ctx)
	if err != nil {
		t.Fatalf("ScanNow: %v", err)
	}
	var f *integrity.SourceCompleteness
	for i := range findings {
		if findings[i].SourceID == "srcS" {
			f = &findings[i]
		}
	}
	if f == nil {
		t.Fatal("srcS missing from ScanNow findings")
	}
	// Settled: gap promoted → status ok, known_loss=1, contiguous zipped to 5.
	if f.Status != integrity.StatusOK {
		t.Fatalf("status=%s after reconcile, want ok (gap promoted to known-loss)", f.Status)
	}
	if f.KnownLoss != 1 {
		t.Fatalf("known_loss_count=%d, want 1", f.KnownLoss)
	}
	if f.Contiguous != 5 {
		t.Fatalf("contiguous=%d, want 5 (advanced past ledgered seq 4)", f.Contiguous)
	}

	// ScanNow with no scanner wired must error, not panic.
	bare := NewWorker(&mockODSReader{}, &mockDWDWriter{}, &mockCheckpointStore{}, NewEnricher(&mockControlReader{}, nil))
	if _, err := bare.ScanNow(ctx); err == nil {
		t.Fatal("ScanNow with no scanner should error")
	}
}
