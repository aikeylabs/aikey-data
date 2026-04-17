package projector

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// --- mocks shared with worker tests ---

type mockODSReader struct {
	pending        []ODSRecord
	markProjected  []int64
	markRetry      []int64
	markDeadLetter []int64
}

func (m *mockODSReader) FetchPending(_ context.Context, _ int) ([]ODSRecord, error) {
	out := m.pending
	m.pending = nil
	return out, nil
}

func (m *mockODSReader) MarkProjected(_ context.Context, odsID int64) error {
	m.markProjected = append(m.markProjected, odsID)
	return nil
}

func (m *mockODSReader) MarkRetry(_ context.Context, odsID int64, _ int, _, _ string) error {
	m.markRetry = append(m.markRetry, odsID)
	return nil
}

func (m *mockODSReader) MarkDeadLetter(_ context.Context, odsID int64, _, _ string) error {
	m.markDeadLetter = append(m.markDeadLetter, odsID)
	return nil
}

type mockDWDWriter struct {
	inserted []string
	failNext error
}

func (m *mockDWDWriter) Insert(_ context.Context, f *DWDFact) (bool, error) {
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		return false, err
	}
	m.inserted = append(m.inserted, f.EventID)
	return true, nil
}

type mockCheckpointStore struct {
	checkpoint int64
}

func (m *mockCheckpointStore) GetLastScannedOdsID(_ context.Context, _ string) (int64, error) {
	return m.checkpoint, nil
}

func (m *mockCheckpointStore) UpdateCheckpoint(_ context.Context, _ string, odsID int64) error {
	m.checkpoint = odsID
	return nil
}

// TestProjectOne_CanaryShortCircuit verifies that canary events are MarkProjected
// without calling the enricher or DWDWriter. This preserves the end-to-end canary
// observability signal (diagnostics watches ODS.dwd_status='projected' for canary)
// while keeping usage_fact_dwd free of canary pollution.
func TestProjectOne_CanaryShortCircuit(t *testing.T) {
	ods := &mockODSReader{}
	dwd := &mockDWDWriter{}
	cr := &mockControlReader{events: map[string]*ControlEvent{}}
	w := NewWorker(ods, dwd, &mockCheckpointStore{}, NewEnricher(cr))

	canaryRec := &ODSRecord{
		OdsID:         42,
		EventID:       "canary-1776408000",
		EventTime:     time.Now(),
		OccurredAt:    time.Now(),
		OrgID:         "__canary__",
		VirtualKeyID:  sql.NullString{String: "__canary__", Valid: true},
		RequestCount:  1,
		RequestStatus: "success",
	}

	if err := w.projectOne(context.Background(), canaryRec); err != nil {
		t.Fatalf("canary projectOne error: %v", err)
	}
	if len(dwd.inserted) != 0 {
		t.Errorf("canary must NOT be inserted into DWD, got %v", dwd.inserted)
	}
	if len(ods.markProjected) != 1 || ods.markProjected[0] != 42 {
		t.Errorf("expected MarkProjected([42]), got %v", ods.markProjected)
	}
	if w.metrics.Snapshot().Projected != 1 {
		t.Errorf("expected Projected metric=1, got %d", w.metrics.Snapshot().Projected)
	}
}

// TestProjectOne_BusinessEventWritesDWD verifies the regular path still writes
// to DWD and MarkProjected.
func TestProjectOne_BusinessEventWritesDWD(t *testing.T) {
	ods := &mockODSReader{}
	dwd := &mockDWDWriter{}
	cr := &mockControlReader{events: map[string]*ControlEvent{
		"vk1": {
			EventID:       "ce1",
			OrgID:         "org1",
			VirtualKeyID:  "vk1",
			ProviderID:    "prov1",
			Revision:      "rev-001",
			EffectiveFrom: time.Now().Add(-time.Hour),
		},
	}}
	w := NewWorker(ods, dwd, &mockCheckpointStore{}, NewEnricher(cr))

	rec := &ODSRecord{
		OdsID:         7,
		EventID:       "e7",
		EventTime:     time.Now(),
		OccurredAt:    time.Now(),
		OrgID:         "org1",
		VirtualKeyID:  sql.NullString{String: "vk1", Valid: true},
		RequestCount:  1,
		RequestStatus: "success",
	}

	if err := w.projectOne(context.Background(), rec); err != nil {
		t.Fatalf("business projectOne error: %v", err)
	}
	if len(dwd.inserted) != 1 || dwd.inserted[0] != "e7" {
		t.Errorf("expected DWD Insert(['e7']), got %v", dwd.inserted)
	}
	if len(ods.markProjected) != 1 || ods.markProjected[0] != 7 {
		t.Errorf("expected MarkProjected([7]), got %v", ods.markProjected)
	}
}

// TestProjectOne_CanaryAckFailureReturnsError ensures a MarkProjected failure
// on a canary is surfaced (so scanOnce logs it), not silently dropped.
func TestProjectOne_CanaryAckFailureReturnsError(t *testing.T) {
	ods := &failingMarkReader{err: errors.New("db locked")}
	dwd := &mockDWDWriter{}
	w := NewWorker(ods, dwd, &mockCheckpointStore{}, NewEnricher(&mockControlReader{}))

	canaryRec := &ODSRecord{
		OdsID:         9,
		EventID:       "canary-9",
		OrgID:         "__canary__",
		VirtualKeyID:  sql.NullString{String: "__canary__", Valid: true},
		RequestStatus: "success",
		EventTime:     time.Now(),
	}
	if err := w.projectOne(context.Background(), canaryRec); err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(dwd.inserted) != 0 {
		t.Errorf("canary must not reach DWD even on error, got %v", dwd.inserted)
	}
}

type failingMarkReader struct {
	mockODSReader
	err error
}

func (f *failingMarkReader) MarkProjected(_ context.Context, _ int64) error {
	return f.err
}
