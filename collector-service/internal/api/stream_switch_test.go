package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The stream switch declares "everything at or below this seq is TERMINATED".
// It exists because the per-lane split makes each lane start above the old
// single stream's high-water (never-reuse must survive the split), stranding a
// bounded span the server would otherwise wait for and eventually write off.
//
// Runs against the REAL schema (newGapsRepo → the actual migration chain), so
// the idempotency guard is exercised as SQL rather than as a mock's promise.

func postSwitch(t *testing.T, h http.HandlerFunc, org, src string, floor int64) streamSwitchResponse {
	t.Helper()
	body, _ := json.Marshal(streamSwitchRequest{OrgID: org, SourceID: src, Floor: floor})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", "/v1/diagnostics/stream-switch", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp streamSwitchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestStreamSwitch_AdvancesThenIsIdempotent(t *testing.T) {
	repo := newGapsRepo(t)
	h := handleStreamSwitch(repo)
	const org, src = "org-switch", "source-switch"

	first := postSwitch(t, h, org, src, 700)
	if !first.Applied || first.Contiguous != 700 {
		t.Fatalf("first declaration should apply and advance to 700, got %+v", first)
	}

	// A retried declaration must neither double-advance nor be reported as a
	// fresh state change — the client resends on any transport hiccup.
	again := postSwitch(t, h, org, src, 700)
	if again.Applied {
		t.Error("replaying the same floor reported a state change; a retry storm would " +
			"then look like repeated stream switches in the audit trail")
	}
	if again.Contiguous != 700 {
		t.Errorf("contiguous moved on replay: %d", again.Contiguous)
	}

	// An older floor must never pull the watermark backwards: that would
	// re-open a span the server has already accounted for.
	older := postSwitch(t, h, org, src, 500)
	if older.Applied || older.Contiguous != 700 {
		t.Errorf("a lower floor must be a no-op, got %+v", older)
	}
}

// 🔴 The decision this fences: a terminated span is NOT a loss. Filing it in
// usage_known_loss_ledger (which also advances contiguous, so it "works") would
// fabricate loss records and make that ledger untrustworthy — the exact
// pollution this whole change exists to remove.
func TestStreamSwitch_DoesNotWriteKnownLoss(t *testing.T) {
	repo, db := newGapsRepoDB(t)
	const org, src = "org-noloss", "source-noloss"

	postSwitch(t, handleStreamSwitch(repo), org, src, 900)

	// The ledger is the audit record of DATA LOSS. Nothing here was lost.
	var n int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM usage_known_loss_ledger WHERE org_id = ? AND source_id = ?",
		org, src).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the stream switch wrote %d known-loss row(s). Those seqs were never "+
			"issued to an event — recording them as lost fabricates loss and destroys "+
			"the ledger's credibility", n)
	}
}

// A lane may declare its floor before it has ever delivered an event (the
// client switches at startup). If no watermark row is seeded, the first event
// to arrive starts at zero and the terminated span reappears as a gap.
func TestStreamSwitch_SeedsWatermarkForAnUnseenLane(t *testing.T) {
	repo := newGapsRepo(t)
	const org, src = "org-unseen", "source-unseen"

	resp := postSwitch(t, handleStreamSwitch(repo), org, src, 300)
	if resp.Contiguous != 300 {
		t.Fatalf("an unseen lane must still get a watermark at the floor, got %d", resp.Contiguous)
	}
	// And a replay against the freshly seeded row is still a no-op.
	if again := postSwitch(t, handleStreamSwitch(repo), org, src, 300); again.Applied {
		t.Error("replay after seeding reported a state change")
	}
}

func TestStreamSwitch_RejectsNonPositiveFloor(t *testing.T) {
	repo := newGapsRepo(t)
	body, _ := json.Marshal(streamSwitchRequest{OrgID: "o", SourceID: "s", Floor: 0})
	w := httptest.NewRecorder()
	handleStreamSwitch(repo)(w, httptest.NewRequest("POST", "/x", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("floor 0 must be rejected, got %d", w.Code)
	}
}
