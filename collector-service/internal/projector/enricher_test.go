package projector

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// --- mocks ---

type mockControlReader struct {
	events map[string]*ControlEvent // keyed by virtual_key_id
}

func (m *mockControlReader) FindByVirtualKeyAtTime(_ context.Context, vkID string, _ aikeytime.Millis) (*ControlEvent, error) {
	return m.events[vkID], nil
}

// --- tests ---

func TestEnrich_Valid(t *testing.T) {
	cr := &mockControlReader{events: map[string]*ControlEvent{
		"vk1": {
			EventID:      "ce1",
			OrgID:        "org1",
			AccountID:    sql.NullString{String: "acc1", Valid: true},
			SeatID:       "seat1",
			VirtualKeyID: "vk1",
			ProviderID:   "prov1",
			Revision:     "rev-001",
			EffectiveFrom: aikeytime.FromTime(time.Now().Add(-1 * time.Hour)),
		},
	}}
	enricher := NewEnricher(cr)

	rec := &ODSRecord{
		OdsID:         1,
		EventID:       "e1",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		AccountID:     sql.NullString{String: "acc1", Valid: true},
		SeatID:        sql.NullString{String: "seat1", Valid: true},
		VirtualKeyID:  sql.NullString{String: "vk1", Valid: true},
		RequestCount:  1,
		RequestStatus: "success",
	}

	fact, err := enricher.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if fact.QualityStatus != QualityExact {
		t.Errorf("expected exact, got %s", fact.QualityStatus)
	}
	if fact.BillingScope != BillOrgAndUser {
		t.Errorf("expected org_and_user, got %s", fact.BillingScope)
	}
	if fact.UserUsageScope != UsageScopeNormal {
		t.Errorf("expected normal, got %s", fact.UserUsageScope)
	}
	if fact.AnomalyType != AnomalyNone {
		t.Errorf("expected no anomaly, got %s", fact.AnomalyType)
	}
	if fact.ControlEventID != "ce1" {
		t.Errorf("expected control event ce1, got %s", fact.ControlEventID)
	}
	// Fix #2: ControlEventRevision must be the event's Revision, not CredentialRevision
	if fact.ControlEventRevision != "rev-001" {
		t.Errorf("expected control event revision rev-001, got %s", fact.ControlEventRevision)
	}
}

func TestEnrich_NoVirtualKey(t *testing.T) {
	enricher := NewEnricher(&mockControlReader{})
	rec := &ODSRecord{
		OdsID:         2,
		EventID:       "e2",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		RequestCount:  1,
		RequestStatus: "success",
	}

	fact, err := enricher.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if fact.QualityStatus != QualityPartial {
		t.Errorf("expected partial, got %s", fact.QualityStatus)
	}
	if fact.AnomalyType != AnomalyPendingReview {
		t.Errorf("expected pending_review, got %s", fact.AnomalyType)
	}
}

func TestEnrich_NoControlEvent(t *testing.T) {
	cr := &mockControlReader{events: map[string]*ControlEvent{}}
	enricher := NewEnricher(cr)

	rec := &ODSRecord{
		OdsID:         3,
		EventID:       "e3",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		VirtualKeyID:  sql.NullString{String: "vk_unknown", Valid: true},
		RequestCount:  1,
		RequestStatus: "success",
	}

	fact, err := enricher.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if fact.QualityStatus != QualityPartial {
		t.Errorf("expected partial, got %s", fact.QualityStatus)
	}
	if fact.AnomalyType != AnomalyPendingReview {
		t.Errorf("expected pending_review, got %s", fact.AnomalyType)
	}
}

func TestEnrich_OrgMismatch(t *testing.T) {
	cr := &mockControlReader{events: map[string]*ControlEvent{
		"vk1": {
			EventID:       "ce1",
			OrgID:         "org_other", // different org!
			SeatID:        "seat1",
			VirtualKeyID:  "vk1",
			EffectiveFrom: aikeytime.FromTime(time.Now().Add(-1 * time.Hour)),
		},
	}}
	enricher := NewEnricher(cr)

	rec := &ODSRecord{
		OdsID:         4,
		EventID:       "e4",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		VirtualKeyID:  sql.NullString{String: "vk1", Valid: true},
		RequestCount:  1,
		RequestStatus: "success",
	}

	fact, err := enricher.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if fact.QualityStatus != QualityInvalid {
		t.Errorf("expected invalid, got %s", fact.QualityStatus)
	}
	if fact.BillingScope != BillHoldReview {
		t.Errorf("expected hold_for_review, got %s", fact.BillingScope)
	}
}

func TestEnrich_AccountMismatch_LateReport(t *testing.T) {
	cr := &mockControlReader{events: map[string]*ControlEvent{
		"vk1": {
			EventID:       "ce1",
			OrgID:         "org1",
			AccountID:     sql.NullString{String: "acc_different", Valid: true},
			SeatID:        "seat1",
			VirtualKeyID:  "vk1",
			EffectiveFrom: aikeytime.FromTime(time.Now().Add(-1 * time.Hour)),
		},
	}}
	enricher := NewEnricher(cr)

	rec := &ODSRecord{
		OdsID:         5,
		EventID:       "e5",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		AccountID:     sql.NullString{String: "acc1", Valid: true},
		SeatID:        sql.NullString{String: "seat1", Valid: true},
		VirtualKeyID:  sql.NullString{String: "vk1", Valid: true},
		RequestCount:  1,
		RequestStatus: "success",
	}

	fact, err := enricher.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if fact.AnomalyType != AnomalyLateReportAbnormal {
		t.Errorf("expected late_report_abnormal_charge, got %s", fact.AnomalyType)
	}
	if fact.BillingScope != BillOrgOnly {
		t.Errorf("expected org_only, got %s", fact.BillingScope)
	}
	if fact.UserUsageScope != UsageScopeAbnormal {
		t.Errorf("expected abnormal, got %s", fact.UserUsageScope)
	}
}

func TestEnrich_BindingMismatch(t *testing.T) {
	cr := &mockControlReader{events: map[string]*ControlEvent{
		"vk1": {
			EventID:       "ce1",
			OrgID:         "org1",
			AccountID:     sql.NullString{String: "acc1", Valid: true},
			SeatID:        "seat1",
			VirtualKeyID:  "vk1",
			BindingID:     sql.NullString{String: "bind_ce", Valid: true},
			CredentialID:  "cred1",
			ProviderID:    "prov1",
			EffectiveFrom: aikeytime.FromTime(time.Now().Add(-1 * time.Hour)),
		},
	}}
	enricher := NewEnricher(cr)

	rec := &ODSRecord{
		OdsID:         10,
		EventID:       "e10",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		AccountID:     sql.NullString{String: "acc1", Valid: true},
		SeatID:        sql.NullString{String: "seat1", Valid: true},
		VirtualKeyID:  sql.NullString{String: "vk1", Valid: true},
		BindingID:     sql.NullString{String: "bind_different", Valid: true},
		RequestCount:  1,
		RequestStatus: "success",
	}

	fact, err := enricher.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if fact.AnomalyType != AnomalyPendingReview {
		t.Errorf("expected pending_review for binding mismatch, got %s", fact.AnomalyType)
	}
	if fact.BillingScope != BillHoldReview {
		t.Errorf("expected hold_for_review, got %s", fact.BillingScope)
	}
}

func TestEnrich_CredentialMismatch(t *testing.T) {
	cr := &mockControlReader{events: map[string]*ControlEvent{
		"vk1": {
			EventID:       "ce1",
			OrgID:         "org1",
			SeatID:        "seat1",
			VirtualKeyID:  "vk1",
			CredentialID:  "cred_ce",
			ProviderID:    "prov1",
			EffectiveFrom: aikeytime.FromTime(time.Now().Add(-1 * time.Hour)),
		},
	}}
	enricher := NewEnricher(cr)

	rec := &ODSRecord{
		OdsID:         11,
		EventID:       "e11",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		SeatID:        sql.NullString{String: "seat1", Valid: true},
		VirtualKeyID:  sql.NullString{String: "vk1", Valid: true},
		CredentialID:  sql.NullString{String: "cred_different", Valid: true},
		RequestCount:  1,
		RequestStatus: "success",
	}

	fact, err := enricher.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if fact.AnomalyType != AnomalyPendingReview {
		t.Errorf("expected pending_review for credential mismatch, got %s", fact.AnomalyType)
	}
}

func TestEnrich_KeyRevisionMismatch(t *testing.T) {
	cr := &mockControlReader{events: map[string]*ControlEvent{
		"vk1": {
			EventID:            "ce1",
			OrgID:              "org1",
			AccountID:          sql.NullString{String: "acc1", Valid: true},
			SeatID:             "seat1",
			VirtualKeyID:       "vk1",
			VirtualKeyRevision: "rev-2",
			ProviderID:         "prov1",
			EffectiveFrom:      aikeytime.FromTime(time.Now().Add(-1 * time.Hour)),
		},
	}}
	enricher := NewEnricher(cr)

	rec := &ODSRecord{
		OdsID:              12,
		EventID:            "e12",
		EventTime:          aikeytime.Now(),
		OccurredAt:         aikeytime.Now(),
		OrgID:              "org1",
		AccountID:          sql.NullString{String: "acc1", Valid: true},
		SeatID:             sql.NullString{String: "seat1", Valid: true},
		VirtualKeyID:       sql.NullString{String: "vk1", Valid: true},
		VirtualKeyRevision: sql.NullString{String: "rev-1", Valid: true}, // stale
		RequestCount:       1,
		RequestStatus:      "success",
	}

	fact, err := enricher.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if fact.AnomalyType != AnomalyLateReportAbnormal {
		t.Errorf("expected late_report for stale key revision, got %s", fact.AnomalyType)
	}
	if fact.ValidationCode != "stale_key_revision" {
		t.Errorf("expected validation_code=stale_key_revision, got %s", fact.ValidationCode)
	}
	// Stale revision is still billable to user (real usage happened)
	if fact.BillingScope != BillOrgAndUser {
		t.Errorf("expected org_and_user, got %s", fact.BillingScope)
	}
}

func TestEnrich_SeatEnrichedFromControlEvent(t *testing.T) {
	cr := &mockControlReader{events: map[string]*ControlEvent{
		"vk1": {
			EventID:       "ce1",
			OrgID:         "org1",
			SeatID:        "seat_from_ce",
			VirtualKeyID:  "vk1",
			ProviderID:    "prov1",
			EffectiveFrom: aikeytime.FromTime(time.Now().Add(-1 * time.Hour)),
		},
	}}
	enricher := NewEnricher(cr)

	rec := &ODSRecord{
		OdsID:         6,
		EventID:       "e6",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		VirtualKeyID:  sql.NullString{String: "vk1", Valid: true},
		RequestCount:  1,
		RequestStatus: "success",
		// SeatID is missing
	}

	fact, err := enricher.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if fact.SeatID != "seat_from_ce" {
		t.Errorf("expected seat enriched from CE, got %s", fact.SeatID)
	}
	if fact.ProviderID != "prov1" {
		t.Errorf("expected provider enriched from CE, got %s", fact.ProviderID)
	}
	if fact.QualityStatus != QualityCompletedFromControlEvent {
		t.Errorf("expected completed_from_control_event, got %s", fact.QualityStatus)
	}
}
