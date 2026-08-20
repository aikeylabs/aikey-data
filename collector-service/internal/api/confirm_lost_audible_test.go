package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ledgering a seq as known-loss is PERMANENT destruction of a billable event,
// and there are two paths that do it. The projector's timeout promotion has
// warned since it was written; this one — the client declaring seqs
// unrecoverable — shipped silent. On 2026-08-20 a single client wrote off 768
// seqs on a live machine and left no line in the server log to find them by;
// the loss was only discoverable by noticing that a row count had stopped
// moving. See bugfix 20260820-usage-delivery-org-partitioned-seq-stream.md.
//
// Fences the CONTRACT, not the call site: whatever the handler does, a
// promotion must be audible at WARN under the SAME event.name the projector
// uses, so one query finds every loss regardless of who declared it. 能红:
// delete the slog.Warn in handleConfirmLost.

func postConfirmLost(t *testing.T, h http.HandlerFunc, org, src string, seqs []int64) (string, *httptest.ResponseRecorder) {
	t.Helper()
	body, _ := json.Marshal(confirmLostRequest{OrgID: org, SourceID: src, Seqs: seqs})
	req := httptest.NewRequest("POST", "/v1/diagnostics/confirm-lost", bytes.NewReader(body))
	w := httptest.NewRecorder()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	h(w, req)
	return buf.String(), w
}

func TestConfirmLost_PromotionIsAudible(t *testing.T) {
	repo := newGapsRepo(t)
	const org, src = "org-audible", "source-audible"
	// Deliver 1 and 4 so 2 and 3 are genuinely absent and therefore promotable.
	seedSource(t, repo, org, src, []int64{1, 4})

	out, w := postConfirmLost(t, handleConfirmLost(repo), org, src, []int64{2, 3})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp confirmLostResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Promoted == 0 {
		t.Fatalf("expected the absent seqs to be promoted, got %+v — test setup no longer exercises a loss", resp)
	}
	for _, want := range []string{"integrity.known_loss.promoted", "client_confirmed", src} {
		if !strings.Contains(out, want) {
			t.Errorf("known-loss WARN must carry %q so the loss is findable; log was:\n%s", want, out)
		}
	}
}

func TestConfirmLost_NoPromotionStaysQuiet(t *testing.T) {
	repo := newGapsRepo(t)
	const org, src = "org-quiet", "source-quiet"
	seedSource(t, repo, org, src, []int64{1, 2})

	// Seq 2 is present in ODS, so the server refuses to mark it lost. Nothing
	// was destroyed — warning here would train operators to ignore the event.
	out, w := postConfirmLost(t, handleConfirmLost(repo), org, src, []int64{2})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(out, "integrity.known_loss.promoted") {
		t.Fatalf("nothing was promoted, so nothing should be logged as lost:\n%s", out)
	}
}
