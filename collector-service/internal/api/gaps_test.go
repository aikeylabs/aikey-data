package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	_ "modernc.org/sqlite"
)

// newGapsRepo builds an ingest ODS repository on a real ComponentData schema
// (in-memory SQLite). Failure-path tests for the D3 reconciliation handlers
// run against the same schema production uses, not an inlined fake table.
func newGapsRepo(t *testing.T) ingest.ODSRepository {
	t.Helper()
	repo, _ := newGapsRepoDB(t)
	return repo
}

// newGapsRepoDB is newGapsRepo plus the raw handle, for assertions that must
// look at a table the repository interface does not expose (e.g. proving the
// stream switch did NOT touch usage_known_loss_ledger).
func newGapsRepoDB(t *testing.T) (ingest.ODSRepository, *sql.DB) {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := versions.UpgradeComponentsTo(context.Background(), raw,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ingest.NewSQLODSRepository(shared.NewDB(raw, shared.DialectSQLite)), raw
}

// seedSource inserts the given seqs for (org, src) and advances the watermark,
// so max_seen / client_allocated reflect what the source has delivered.
func seedSource(t *testing.T, r ingest.ODSRepository, org, src string, seqs []int64) {
	t.Helper()
	for _, seq := range seqs {
		s := seq
		now := aikeytime.Now()
		e := &ingest.UsageEvent{
			EventID: org + "-" + src + "-" + string(rune('0'+seq)), OrgID: org, SourceID: src, SourceSeq: &s,
			EventTime: now, OccurredAt: now, RequestStatus: "success", RequestCount: 1,
		}
		if _, _, err := r.InsertEvent(context.Background(), e, []byte("{}"), false); err != nil {
			t.Fatalf("seed insert seq=%d: %v", seq, err)
		}
	}
	if _, err := r.AdvanceWatermark(context.Background(), org, src, 0); err != nil {
		t.Fatalf("advance watermark: %v", err)
	}
}

// errGapsRepo fails every call — used to assert the handlers translate a
// repository/DB error into HTTP 500 rather than a 200 with a bogus body.
type errGapsRepo struct{ err error }

func (e errGapsRepo) GapSeqs(context.Context, string, string, int64) ([]int64, bool, error) {
	return nil, false, e.err
}
func (e errGapsRepo) ConfirmLost(context.Context, string, string, []int64, string) (int, int64, error) {
	return 0, 0, e.err
}

func (e errGapsRepo) ApplyStreamFloor(context.Context, string, string, int64) (int64, bool, error) {
	return 0, false, e.err
}

// ── handleGaps validation (HTTP contract) ───────────────────────────────

func TestHandleGaps_MissingOrgID_400(t *testing.T) {
	rec := httptest.NewRecorder()
	handleGaps(nil)(rec, httptest.NewRequest(http.MethodGet, "/v1/diagnostics/gaps?source_id=srcX", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (org_id required)", rec.Code)
	}
}

func TestHandleGaps_MissingSourceID_400(t *testing.T) {
	rec := httptest.NewRecorder()
	handleGaps(nil)(rec, httptest.NewRequest(http.MethodGet, "/v1/diagnostics/gaps?org_id=orgX", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (source_id required)", rec.Code)
	}
}

func TestHandleGaps_RepoError_500(t *testing.T) {
	rec := httptest.NewRecorder()
	handleGaps(errGapsRepo{err: sql.ErrConnDone})(rec,
		httptest.NewRequest(http.MethodGet, "/v1/diagnostics/gaps?org_id=o&source_id=s", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (repo error surfaced)", rec.Code)
	}
}

// ── handleConfirmLost validation (HTTP contract) ────────────────────────

func TestHandleConfirmLost_InvalidBody_400(t *testing.T) {
	rec := httptest.NewRecorder()
	handleConfirmLost(nil)(rec, httptest.NewRequest(http.MethodPost, "/v1/diagnostics/confirm-lost",
		strings.NewReader("{not-json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (invalid body)", rec.Code)
	}
}

func TestHandleConfirmLost_MissingIdentity_400(t *testing.T) {
	rec := httptest.NewRecorder()
	handleConfirmLost(nil)(rec, httptest.NewRequest(http.MethodPost, "/v1/diagnostics/confirm-lost",
		strings.NewReader(`{"seqs":[1,2]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (org_id/source_id required)", rec.Code)
	}
}

func TestHandleConfirmLost_RepoError_500(t *testing.T) {
	rec := httptest.NewRecorder()
	handleConfirmLost(errGapsRepo{err: sql.ErrConnDone})(rec,
		httptest.NewRequest(http.MethodPost, "/v1/diagnostics/confirm-lost",
			strings.NewReader(`{"org_id":"o","source_id":"s","seqs":[1]}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (repo error surfaced)", rec.Code)
	}
}

// ── handleConfirmLost integrity guards (billing-loss inflation) ──────────

// A seq the client never allocated and the server never saw must NOT be
// promoted to known_loss — otherwise a buggy/malicious client could inflate
// known_loss (and advance the contiguous high-water) with fabricated seqs.
func TestHandleConfirmLost_RejectsAboveCeilingSeq(t *testing.T) {
	repo := newGapsRepo(t)
	seedSource(t, repo, "orgG", "srcG", []int64{1, 2, 3}) // ceiling = 3

	body := `{"org_id":"orgG","source_id":"srcG","seqs":[999]}`
	rec := httptest.NewRecorder()
	handleConfirmLost(repo)(rec, httptest.NewRequest(http.MethodPost, "/v1/diagnostics/confirm-lost",
		strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp confirmLostResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Promoted != 0 {
		t.Fatalf("promoted=%d, want 0 (fabricated above-ceiling seq must not be ledgered)", resp.Promoted)
	}
}

// A seq already received (present in ODS) must never be marked lost.
func TestHandleConfirmLost_DoesNotPromotePresentSeq(t *testing.T) {
	repo := newGapsRepo(t)
	seedSource(t, repo, "orgP", "srcP", []int64{1, 2, 3}) // seq 2 is present

	body := `{"org_id":"orgP","source_id":"srcP","seqs":[2]}`
	rec := httptest.NewRecorder()
	handleConfirmLost(repo)(rec, httptest.NewRequest(http.MethodPost, "/v1/diagnostics/confirm-lost",
		strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp confirmLostResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Promoted != 0 {
		t.Fatalf("promoted=%d, want 0 (a present seq must not be marked lost)", resp.Promoted)
	}
}
