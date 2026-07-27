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
	enricher := NewEnricher(cr, nil)

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

// TestEnrich_SessionIDPropagatesFromODSToDWD pins the v1.0.0-rc.6
// projector contract: whatever session_id the proxy wrote into ODS
// must appear verbatim in DWD, with no enrichment, no fallback,
// no rewriting. This guards the design doc's "single source of truth
// is the proxy extractor" invariant — drift would mean a future
// reader of by-session aggregates couldn't trust they reflect what
// the client actually sent.
//
// Two cases in one test: a present session_id flows through, and an
// absent (NULL) session_id surfaces as empty string on DWD (the
// downstream by-session SQL coalesces both to '').
func TestEnrich_SessionIDPropagatesFromODSToDWD(t *testing.T) {
	enricher := NewEnricher(&mockControlReader{}, nil)

	withSession := &ODSRecord{
		OdsID:         101,
		EventID:       "evt-with-session",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		RequestCount:  1,
		RequestStatus: "success",
		SessionID:     sql.NullString{String: "claude-session-abc123", Valid: true},
	}
	fact, err := enricher.Enrich(context.Background(), withSession)
	if err != nil {
		t.Fatal(err)
	}
	if fact.SessionID != "claude-session-abc123" {
		t.Errorf("session_id not propagated: want %q, got %q", "claude-session-abc123", fact.SessionID)
	}

	withoutSession := &ODSRecord{
		OdsID:         102,
		EventID:       "evt-no-session",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		RequestCount:  1,
		RequestStatus: "success",
		// SessionID intentionally zero-value (NullString.Valid = false)
	}
	fact2, err := enricher.Enrich(context.Background(), withoutSession)
	if err != nil {
		t.Fatal(err)
	}
	if fact2.SessionID != "" {
		t.Errorf("missing session_id should propagate as empty string, got %q", fact2.SessionID)
	}
}

func TestEnrich_RequestIDPropagatesFromODSToDWD(t *testing.T) {
	enricher := NewEnricher(&mockControlReader{}, nil)
	rec := &ODSRecord{
		OdsID:         103,
		EventID:       "evt-with-request-id",
		RequestID:     sql.NullString{String: "req-b-to-a", Valid: true},
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
	if fact.RequestID != "req-b-to-a" {
		t.Fatalf("request_id not propagated: got %q, want req-b-to-a", fact.RequestID)
	}
}

// TestEnrich_IntegrityColumnsPropagateFromODSToDWD pins the v1.0.1-alpha.3
// projector contract: content_hash / source_id / source_seq written on ODS
// (since rc.7) must appear verbatim in DWD so the enterprise usage-audit
// export carries tamper/gap evidence without joining ODS. Three points:
//   - present values flow through unchanged
//   - a NULL source_seq (old-proxy event) stays NULL on DWD (*int64 nil),
//     NOT coerced to 0 — the export must distinguish "no sequence" from seq 0
//   - absent content_hash/source_id surface as "" (same convention as AppSlug)
func TestEnrich_IntegrityColumnsPropagateFromODSToDWD(t *testing.T) {
	enricher := NewEnricher(&mockControlReader{}, nil)

	withIntegrity := &ODSRecord{
		OdsID:         201,
		EventID:       "evt-with-integrity",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		RequestCount:  1,
		RequestStatus: "success",
		ContentHash:   sql.NullString{String: "sha256:v1:abc123", Valid: true},
		SourceID:      sql.NullString{String: "src-uuid-1", Valid: true},
		SourceSeq:     sql.NullInt64{Int64: 1024, Valid: true},
	}
	fact, err := enricher.Enrich(context.Background(), withIntegrity)
	if err != nil {
		t.Fatal(err)
	}
	if fact.ContentHash != "sha256:v1:abc123" {
		t.Errorf("content_hash not propagated: got %q", fact.ContentHash)
	}
	if fact.SourceID != "src-uuid-1" {
		t.Errorf("source_id not propagated: got %q", fact.SourceID)
	}
	if fact.SourceSeq == nil || *fact.SourceSeq != 1024 {
		t.Errorf("source_seq not propagated: got %v", fact.SourceSeq)
	}

	// Old-proxy event: source_seq NULL must stay NULL (nil), content_hash/
	// source_id absent must surface as "".
	legacy := &ODSRecord{
		OdsID:         202,
		EventID:       "evt-legacy-no-seq",
		EventTime:     aikeytime.Now(),
		OccurredAt:    aikeytime.Now(),
		OrgID:         "org1",
		RequestCount:  1,
		RequestStatus: "success",
		// ContentHash / SourceID / SourceSeq intentionally zero-value (NULL)
	}
	fact2, err := enricher.Enrich(context.Background(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if fact2.SourceSeq != nil {
		t.Errorf("NULL source_seq must stay nil (not coerced to 0), got %v", *fact2.SourceSeq)
	}
	if fact2.ContentHash != "" || fact2.SourceID != "" {
		t.Errorf("absent content_hash/source_id should be empty string, got %q / %q", fact2.ContentHash, fact2.SourceID)
	}
}

func TestEnrich_NoVirtualKey(t *testing.T) {
	enricher := NewEnricher(&mockControlReader{}, nil)
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
	enricher := NewEnricher(cr, nil)

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
	enricher := NewEnricher(cr, nil)

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
	enricher := NewEnricher(cr, nil)

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
	enricher := NewEnricher(cr, nil)

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
	enricher := NewEnricher(cr, nil)

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
	enricher := NewEnricher(cr, nil)

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
	enricher := NewEnricher(cr, nil)

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

// TestEnrich_VaultOriginVKShortCircuit is the regression guard for
// bugfix 20260522-projector-stuck-mark-dead-letter-param-order.md.
//
// All VKs with vault-origin prefixes (personal: / oauth: / app:) must
// short-circuit BEFORE the controlReader.FindByVirtualKeyAtTime call,
// otherwise Personal SQLite installs would raise "no such table:
// managed_key_control_events" and the projector worker would stall in
// retry loop. This test uses a panicOnLookupReader sentinel that fails
// the test loudly on any DB call — if the short-circuit is removed or
// a prefix is missed, the test will surface the regression immediately
// rather than letting it silently re-introduce the projector stall.
func TestEnrich_VaultOriginVKShortCircuit(t *testing.T) {
	sentinel := &panicOnLookupReader{t: t}
	enricher := NewEnricher(sentinel, nil)

	cases := []struct {
		name string
		vk   string
	}{
		{"personal prefix", "personal:my-claude"},
		{"oauth prefix", "oauth:session_abc123"},
		{"app prefix", "app:degrade-detector"},
		// probe: prefix added BR-rc.5-54 (2026-05-25). Mode C probe
		// events were silently stalling in retry loop on Personal
		// edition because probe: wasn't in vaultOriginVKPrefixes —
		// the projector tried managed_key_control_events lookup,
		// table doesn't exist on Personal, ENRICH_FAILED. This sub-
		// case is the fence that catches the regression if anyone
		// removes probe: from vaultOriginVKPrefixes without
		// understanding the consequences.
		{"probe prefix", "probe:FreySilvaqzs@qualityservice.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &ODSRecord{
				OdsID:         1,
				EventID:       "e-" + c.name,
				EventTime:     aikeytime.Now(),
				OccurredAt:    aikeytime.Now(),
				OrgID:         "personal",
				VirtualKeyID:  sql.NullString{String: c.vk, Valid: true},
				RequestCount:  1,
				RequestStatus: "success",
			}
			fact, err := enricher.Enrich(context.Background(), rec)
			if err != nil {
				t.Fatalf("Enrich must succeed for vault-origin VK %q (short-circuit), got error: %v", c.vk, err)
			}
			if fact.CompletionSource != "personal_vault_key" {
				t.Errorf("CompletionSource: want 'personal_vault_key' (DWD enum backwards-compat), got %q", fact.CompletionSource)
			}
			if fact.QualityStatus != QualityPartial {
				t.Errorf("QualityStatus: want partial, got %s", fact.QualityStatus)
			}
			if fact.AnomalyType != AnomalyNone {
				t.Errorf("AnomalyType: want none (vault-origin VKs aren't anomalies), got %s", fact.AnomalyType)
			}
		})
	}
}

// panicOnLookupReader fails the test loudly if anyone calls
// FindByVirtualKeyAtTime — used by TestEnrich_VaultOriginVKShortCircuit
// to prove the enricher never reached the DB for vault-origin VKs.
type panicOnLookupReader struct{ t *testing.T }

func (p *panicOnLookupReader) FindByVirtualKeyAtTime(ctx context.Context, vk string, _ aikeytime.Millis) (*ControlEvent, error) {
	p.t.Fatalf("enricher tried to look up vault-origin VK %q in managed_key_control_events — short-circuit missed", vk)
	return nil, nil
}
