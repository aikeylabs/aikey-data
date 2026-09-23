package ingest

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/lib/pq"
)

// Fences for bugfix 2026-09-23-collector-data-error-classified-transient.
//
// Production shape (worker-1, 2026-09-09 → 09-23): one event carried a
// 2049-character error_message, PostgreSQL rejected the INSERT with SQLSTATE
// 22001, insertEventOn wrapped it as TransientStorageError, the handler answered
// 503 for the whole batch, and the proxy re-sent the identical batch every 30 s
// for two weeks — its confirmed watermark stuck, its WAL prune starved. Three
// fences: (1) a deterministic PG data error is terminal, not transient; (2)
// transient classes stay transient; (3) diagnostic text is clamped to the
// column so the stored row keeps every billing field; (4) the clamp width is
// pinned to the DDL.
//
// PG data errors cannot be produced on the SQLite test substrate (TEXT columns),
// so the driver error is injected through the Execer seam insertEventOn already
// takes for the batch-transaction path.

type failingExecer struct {
	shared.Execer
	err error
}

func (f failingExecer) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, f.err
}

func TestInsertEventOn_DeterministicPGErrorIsTerminalNotTransient(t *testing.T) {
	db := newWatermarkTestDB(t)
	ev := batchEvent("orgTD", "srcTD", "td-1", 1)
	inserted, conflict, infra, err := insertEventOn(context.Background(), db,
		failingExecer{Execer: db, err: &pq.Error{Code: "22001", Message: "value too long for type character varying(1024)"}},
		&ev, []byte("{}"), false)
	if inserted || conflict {
		t.Fatalf("a failed INSERT must report neither inserted nor conflict")
	}
	if !infra {
		t.Fatalf("infra must stay true: inside a PostgreSQL tx the failed statement poisons the tx regardless of why, the batch writer must roll back and replay per event")
	}
	var tde *shared.TerminalDataError
	if !errors.As(err, &tde) || tde.SQLState != "22001" {
		t.Fatalf("SQLSTATE 22001 must surface as *shared.TerminalDataError{22001}, got %T %v", err, err)
	}
	var tse *TransientStorageError
	if errors.As(err, &tse) {
		t.Fatalf("a deterministic data error must NOT be transient — that classification is what looped the proxy for two weeks")
	}
}

func TestInsertEventOn_NonDeterministicErrorsStayTransient(t *testing.T) {
	db := newWatermarkTestDB(t)
	for _, injected := range []error{
		&pq.Error{Code: "08006"},                   // connection failure
		&pq.Error{Code: "40001"},                   // serialization failure
		&pq.Error{Code: "53300"},                   // too many connections
		errors.New("constraint failed: simulated"), // what SQLite's RAISE(ABORT) looks like
		context.DeadlineExceeded,
	} {
		ev := batchEvent("orgTT", "srcTT", "tt-1", 1)
		_, _, infra, err := insertEventOn(context.Background(), db, failingExecer{Execer: db, err: injected}, &ev, []byte("{}"), false)
		var tse *TransientStorageError
		if !infra || !errors.As(err, &tse) {
			t.Fatalf("%v must stay transient (infra=%v err=%T): P0-4's whole-batch 503 is the only signal the proxy can act on", injected, infra, err)
		}
		var tde *shared.TerminalDataError
		if errors.As(err, &tde) {
			t.Fatalf("%v must not be classified terminal", injected)
		}
	}
}

// terminalMock poisons one event with a deterministic data error; everything
// else behaves like the in-memory mock the service tests already use.
type terminalMock struct {
	*mockODS
	poison string
}

func (m *terminalMock) InsertEvent(ctx context.Context, e *UsageEvent, raw []byte, quarantined bool) (bool, bool, error) {
	if e.OrgID+"/"+e.EventID == m.poison {
		return false, false, &shared.TerminalDataError{SQLState: "22001", Err: errors.New("pq: value too long for type character varying(1024)")}
	}
	return m.mockODS.InsertEvent(ctx, e, raw, quarantined)
}

func TestIngestBatch_TerminalDataError_IsPerEventRejectedNot503(t *testing.T) {
	repo := &terminalMock{mockODS: newMockODS(), poison: "orgTD/td-poison"}
	svc := NewService(repo)
	resp, results := svc.IngestBatch(context.Background(), &BatchRequest{Events: []UsageEvent{
		batchEvent("orgTD", "srcTD", "td-e1", 1),
		batchEvent("orgTD", "srcTD", "td-poison", 2),
		batchEvent("orgTD", "srcTD", "td-e2", 3),
	}})
	if HasTransientFailure(results) {
		t.Fatalf("a deterministic data error must not trip the whole-batch 503 predicate")
	}
	if resp.Accepted != 2 || resp.Rejected != 1 {
		t.Fatalf("want accepted=2 rejected=1, got accepted=%d rejected=%d", resp.Accepted, resp.Rejected)
	}
	var found bool
	for _, r := range results {
		if r.EventID == "td-poison" {
			found = true
			if r.Status != "rejected" || r.Reason != "invalid_data:22001" {
				t.Fatalf("poisoned event: want rejected/invalid_data:22001, got %s/%s", r.Status, r.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("poisoned event missing from results")
	}
}

func TestInsertEvent_ErrorMessageClampedToColumnWidth(t *testing.T) {
	db := newWatermarkTestDB(t)
	repo := NewSQLODSRepository(db)
	ev := batchEvent("orgCL", "srcCL", "cl-1", 1)
	ev.RequestStatus = "error"
	ev.ErrorMessage = strings.Repeat("é", 2049) // 2 bytes per char: byte-length ≠ char-length on purpose
	inserted, _, err := repo.InsertEvent(context.Background(), &ev, []byte("{}"), false)
	if err != nil || !inserted {
		t.Fatalf("insert with an over-long error_message must succeed after clamping (inserted=%v err=%v)", inserted, err)
	}
	var stored string
	if err := db.QueryRowContext(context.Background(),
		"SELECT error_message FROM usage_event_ods WHERE org_id = ? AND event_id = ?", "orgCL", "cl-1").Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n := utf8.RuneCountInString(stored); n != odsErrorMessageChars {
		t.Fatalf("stored error_message must be clamped to %d chars, got %d", odsErrorMessageChars, n)
	}
	if !utf8.ValidString(stored) {
		t.Fatalf("clamp split a UTF-8 sequence")
	}
}

// TestODSErrorMessageWidthMatchesDDL pins odsErrorMessageChars to the two DDL
// sources (migration + Postgres baseline): the DDL is the truth, the constant is
// its mirror, and widening one without the other must fail here, not in
// production as a fresh 22001 loop.
func TestODSErrorMessageWidthMatchesDDL(t *testing.T) {
	re := regexp.MustCompile(`error_message\s+VARCHAR\((\d+)\)`)
	for _, p := range []string{
		filepath.Join("..", "..", "migrations", "001_usage_event_ods.sql"),
		filepath.Join("..", "..", "..", "baseline", "data_postgres.go"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read DDL %s: %v", p, err)
		}
		m := re.FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s: no `error_message VARCHAR(n)` column found", p)
		}
		n, _ := strconv.Atoi(string(m[1]))
		if n != odsErrorMessageChars {
			t.Fatalf("%s declares error_message VARCHAR(%d) but odsErrorMessageChars = %d — keep them equal (and aikey-proxy's errorBodyCap below it)", p, n, odsErrorMessageChars)
		}
	}
}
