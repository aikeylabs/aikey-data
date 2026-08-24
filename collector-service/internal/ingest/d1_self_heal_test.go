package ingest

import (
	"context"
	"testing"
)

// Self-heal: after D1a the collector reports the REAL gaps, which is the input
// the proxy's existing reconciler needs to resend what it is holding.
//
// 🔴 WHY THIS IS THE TEST THAT MATTERS (2026-08-24). Nothing in the recovery
// path was missing — the proxy already runs ReconcileGaps, which "enumerates
// the exact missing seqs [and] WAL-present ones are re-sent, bypassing the
// sentSeq filter". On winpc2 it ran and reported resent=0 still_missing=0: it
// asked what was missing and the collector answered "nothing", because the
// org-partitioned ledger had written the real sequences off as known-loss and
// pushed contiguity past them.
//
// So the fix is not a repair script. It is making the collector tell the truth,
// after which the EXISTING machinery recovers on its own — which is what this
// project requires of a privately deployed product: 自愈, not manual repair.
//
// This test pins that: given the historic broken state, gap enumeration must
// name the genuinely-missing sequence and NOT the ones delivered under the
// other org.
func TestD1SelfHeal_GapEnumerationNamesRealGapsOnly(t *testing.T) {
	db := newWatermarkTestDB(t)
	repo := NewSQLODSRepository(db)
	ctx := context.Background()

	const src = "385b7a53"

	// The historic split: one proxy's stream 1..6, attributed alternately, plus
	// seq 7 that genuinely never arrived anywhere.
	for _, seq := range []int64{1, 3, 5} {
		insertSeq(t, repo, "personal", src, seq)
	}
	for _, seq := range []int64{2, 4, 6} {
		insertSeq(t, repo, "624a2488", src, seq)
	}

	// The blinded ledgers wrote off each other's sequences — the rows that made
	// the collector claim completeness. They are left in place ON PURPOSE: the
	// point is that self-heal works WITHOUT deleting them.
	if _, err := repo.RecordKnownLoss(ctx, "personal", src, []int64{2, 4, 6}, "gap"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecordKnownLoss(ctx, "624a2488", src, []int64{1, 3, 5}, "gap"); err != nil {
		t.Fatal(err)
	}

	missing, err := repo.EnumerateMissingSeqs(ctx, "personal", src, 0, 7)
	if err != nil {
		t.Fatalf("EnumerateMissingSeqs: %v", err)
	}

	if len(missing) != 1 || missing[0] != 7 {
		t.Fatalf("missing = %v, want [7]\n\n"+
			"An empty result is the winpc2 failure: the collector says 'nothing missing',\n"+
			"the proxy's reconciler has nothing to resend, and usage stops appearing.\n"+
			"A result containing 1..6 would be the opposite error: sequences that WERE\n"+
			"delivered (under the other org) reported as gaps, which the reconciler would\n"+
			"eventually confirm-lost — destroying real billing data.", missing)
	}

	t.Logf("self-heal input verified: stale known-loss rows left in place, and the "+
		"collector still names exactly the real gap %v. The proxy's existing "+
		"ReconcileGaps resends WAL-present seqs from here — no repair script, no "+
		"manual step, no data mutation.", missing)
}
