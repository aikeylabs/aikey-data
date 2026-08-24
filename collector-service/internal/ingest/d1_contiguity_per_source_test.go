package ingest

import (
	"context"
	"testing"
)

// D1 reproduction: ONE proxy's sequence stream, split across TWO org ledgers,
// blinds both halves — each sees the other's sequence numbers as gaps.
//
// 🔴 This began as a REPRODUCTION (it asserted the broken numbers) and became
// this guard when D1a landed on 2026-08-24. Before the fix it measured
// personal=1, team=0 on a stream with no gaps at all; now both reach 6.
//
// 🔴 FIELD EVIDENCE (winpc2, 2026-08-24). GET /v1/diagnostics/completeness
// returned two rows for the SAME source_id:
//
//	org_id=personal    contiguous_seq=2082  max_seen_seq=1827  known_loss=2075
//	org_id=624a2488-…  contiguous_seq=1032  max_seen_seq=1032  known_loss=1031
//
// contiguous_seq ABOVE max_seen_seq is the signature: the only way a watermark
// passes sequences it never saw is RecordKnownLoss → AdvanceWatermark. Those
// 2075 "lost" sequences were not lost — they were delivered to the OTHER
// ledger. Downstream the proxy's sentSeq advanced past events the collector
// never recorded, so new usage stopped appearing while traffic flowed fine.
func TestD1_ContiguityIsPerSourceNotPerOrg(t *testing.T) {
	db := newWatermarkTestDB(t)
	repo := NewSQLODSRepository(db)
	ctx := context.Background()

	const src = "385b7a53" // one proxy = one source_id, as the field data shows
	const orgPersonal = "personal"
	const orgTeam = "624a2488"

	// ONE allocator hands out 1..6 without gaps. Odd seqs are attributed to the
	// personal lane, even ones to the team lane — which is exactly what a
	// machine with both a personal OAuth credential and a team key produces.
	for _, seq := range []int64{1, 3, 5} {
		insertSeq(t, repo, orgPersonal, src, seq)
	}
	for _, seq := range []int64{2, 4, 6} {
		insertSeq(t, repo, orgTeam, src, seq)
	}

	personal, err := repo.AdvanceWatermark(ctx, orgPersonal, src, 0)
	if err != nil {
		t.Fatalf("AdvanceWatermark(personal): %v", err)
	}
	team, err := repo.AdvanceWatermark(ctx, orgTeam, src, 0)
	if err != nil {
		t.Fatalf("AdvanceWatermark(team): %v", err)
	}

	// The stream is COMPLETE: 1..6 all arrived, nothing was lost. A ledger that
	// accounted the stream correctly would report 6 for whoever owns it.
	// The stream is complete, so BOTH views of it must reach the end. A value
	// below 6 means the ledger has re-partitioned by org and is once again
	// treating the other credential's sequences as gaps — the exact state that
	// wrote off 3106 real events on winpc2.
	if personal != 6 {
		t.Errorf("personal contiguous = %d, want 6 — the 1..6 stream has NO gaps; "+
			"anything less means contiguity is being computed per org again", personal)
	}
	if team != 6 {
		t.Errorf("team contiguous = %d, want 6 — same stream, same source_id; "+
			"a per-org split is what blinds the reconciler and turns real events "+
			"into known-loss", team)
	}
}
