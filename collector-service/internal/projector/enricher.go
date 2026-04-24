package projector

import (
	"context"
	"database/sql"
	"log/slog"
)

const projectorVersion = "0.1.0"

// Enricher transforms an ODS record into a DWD fact by looking up control events.
type Enricher struct {
	controlReader ControlEventReader
}

// NewEnricher creates an enricher that reads control events for enrichment (D5).
func NewEnricher(cr ControlEventReader) *Enricher {
	return &Enricher{controlReader: cr}
}

// Enrich takes an ODS record, looks up the matching control event, and produces a DWD fact.
func (e *Enricher) Enrich(ctx context.Context, rec *ODSRecord) (*DWDFact, error) {
	fact := e.buildBaseFact(rec)

	// If no virtual_key_id, we cannot look up control events — mark as partial
	if !rec.VirtualKeyID.Valid || rec.VirtualKeyID.String == "" {
		fact.CompletionSource = "no_virtual_key"
		fact.QualityStatus = QualityPartial
		fact.BillingScope = BillOrgOnly
		fact.UserUsageScope = UsageScopeExcluded
		fact.AnomalyType = AnomalyPendingReview
		fact.AnomalyReason = "missing virtual_key_id, cannot verify ownership"
		return fact, nil
	}

	// Look up the control event effective at event_time (D5: only read managed_key_control_events)
	ce, err := e.controlReader.FindByVirtualKeyAtTime(ctx, rec.VirtualKeyID.String, rec.EventTime)
	if err != nil {
		return nil, err
	}

	if ce == nil {
		// No control event found — cannot verify, mark as pending_review
		fact.CompletionSource = "no_control_event"
		fact.QualityStatus = QualityPartial
		fact.BillingScope = BillOrgOnly
		fact.UserUsageScope = UsageScopeExcluded
		fact.AnomalyType = AnomalyPendingReview
		fact.AnomalyReason = "no control event found for virtual_key_id at event_time"
		return fact, nil
	}

	// Enrich from control event
	e.applyControlEvent(fact, rec, ce)
	return fact, nil
}

func (e *Enricher) buildBaseFact(rec *ODSRecord) *DWDFact {
	return &DWDFact{
		EventID:                    rec.EventID,
		OdsID:                      rec.OdsID,
		OccurredAt:                 rec.OccurredAt,
		EventTime:                  rec.EventTime,
		// UsageDate is the UTC date portion of EventTime ("YYYY-MM-DD").
		// Computed via Millis.Time() (already UTC) so bucket-by-date queries
		// agree with bucket-by-hour queries on event_time millis.
		UsageDate:                  rec.EventTime.Time().Format("2006-01-02"),

		OrgID:                      rec.OrgID,
		AccountID:                  rec.AccountID.String,
		SeatID:                     rec.SeatID.String,

		VirtualKeyID:               rec.VirtualKeyID.String,
		VirtualKeyRevision:         rec.VirtualKeyRevision.String,
		VirtualKeyHash:             rec.VirtualKeyHash.String,

		BindingID:                  rec.BindingID.String,
		CredentialID:               rec.CredentialID.String,
		CredentialRevision:         rec.CredentialRevision.String,
		RealKeyHash:                rec.RealKeyHash.String,
		CredentialFingerprint:      rec.CredentialFingerprint.String,
		ProviderAccountFingerprint: rec.ProviderAccountFingerprint.String,

		ProviderID:                 rec.ProviderID.String,
		ProviderCode:               rec.ProviderCode.String,
		ProtocolType:               rec.ProtocolType.String,
		RouteSource:                rec.RouteSource.String,
		Model:                      rec.Model.String,

		RequestCount:               rec.RequestCount,
		InputTokens:                nullInt64Val(rec.InputTokens),
		OutputTokens:               nullInt64Val(rec.OutputTokens),
		CachedInputTokens:          nullInt64Val(rec.CachedInputTokens),
		ReasoningTokens:            nullInt64Val(rec.ReasoningTokens),
		TotalTokens:                nullInt64Val(rec.TotalTokens),
		BillableAmount:             nullStrPtr(rec.BillableAmount),
		Currency:                   rec.Currency.String,

		RequestStatus:              rec.RequestStatus,
		HTTPStatusCode:             nullInt32Ptr(rec.HTTPStatusCode),
		UpstreamRequestID:          rec.UpstreamRequestID.String,

		ProjectorVersion:           projectorVersion,
	}
}

func (e *Enricher) applyControlEvent(fact *DWDFact, rec *ODSRecord, ce *ControlEvent) {
	fact.ControlEventID = ce.EventID
	fact.ControlEventRevision = ce.Revision // event-level revision, not credential_revision

	// Enrich missing fields from control event
	if fact.SeatID == "" {
		fact.SeatID = ce.SeatID
	}
	if fact.ProviderID == "" {
		fact.ProviderID = ce.ProviderID
	}
	if fact.BindingID == "" && ce.BindingID.Valid {
		fact.BindingID = ce.BindingID.String
	}
	if fact.CredentialID == "" {
		fact.CredentialID = ce.CredentialID
	}

	// --- Ownership checks (ordered by severity, highest first) ---

	// 1. Org mismatch — most serious (D8: pending_review)
	if rec.OrgID != ce.OrgID {
		e.markAnomaly(fact, "control_event_mismatch", QualityInvalid,
			AnomalyPendingReview, "org_id mismatch: ods="+rec.OrgID+" ce="+ce.OrgID,
			BillHoldReview, UsageScopeExcluded)
		slog.Warn("org mismatch in projection",
			"event_id", rec.EventID, "ods_org", rec.OrgID, "ce_org", ce.OrgID)
		return
	}

	// 2. Binding / credential / provider consistency (if ODS carries these fields)
	if rec.BindingID.Valid && ce.BindingID.Valid && rec.BindingID.String != ce.BindingID.String {
		e.markAnomaly(fact, "control_event", QualityCompletedFromControlEvent,
			AnomalyPendingReview, "binding_id mismatch: ods="+rec.BindingID.String+" ce="+ce.BindingID.String,
			BillHoldReview, UsageScopeExcluded)
		return
	}
	if rec.CredentialID.Valid && ce.CredentialID != "" && rec.CredentialID.String != ce.CredentialID {
		e.markAnomaly(fact, "control_event", QualityCompletedFromControlEvent,
			AnomalyPendingReview, "credential_id mismatch: ods="+rec.CredentialID.String+" ce="+ce.CredentialID,
			BillHoldReview, UsageScopeExcluded)
		return
	}
	if rec.ProviderID.Valid && ce.ProviderID != "" && rec.ProviderID.String != ce.ProviderID {
		e.markAnomaly(fact, "control_event", QualityCompletedFromControlEvent,
			AnomalyPendingReview, "provider_id mismatch: ods="+rec.ProviderID.String+" ce="+ce.ProviderID,
			BillHoldReview, UsageScopeExcluded)
		return
	}

	// 3. Account mismatch — late report or abnormal charge
	accountMatch := true
	if rec.AccountID.Valid && ce.AccountID.Valid {
		accountMatch = rec.AccountID.String == ce.AccountID.String
	}

	// 4. Seat mismatch
	seatMatch := true
	if rec.SeatID.Valid && ce.SeatID != "" {
		seatMatch = rec.SeatID.String == ce.SeatID
	}

	if !accountMatch || !seatMatch {
		reason := "account or seat mismatch at event_time"
		if !accountMatch {
			reason = "account_id mismatch: ods=" + rec.AccountID.String + " ce=" + ce.AccountID.String
		} else {
			reason = "seat_id mismatch: ods=" + rec.SeatID.String + " ce=" + ce.SeatID
		}
		e.markAnomaly(fact, "control_event", QualityCompletedFromControlEvent,
			AnomalyLateReportAbnormal, reason,
			BillOrgOnly, UsageScopeAbnormal)
		return
	}

	// 5. Key revision mismatch — proxy used a stale key version
	if rec.VirtualKeyRevision.Valid && ce.VirtualKeyRevision != "" &&
		rec.VirtualKeyRevision.String != ce.VirtualKeyRevision {
		e.markAnomaly(fact, "control_event", QualityCompletedFromControlEvent,
			AnomalyLateReportAbnormal,
			"virtual_key_revision mismatch: ods="+rec.VirtualKeyRevision.String+" ce="+ce.VirtualKeyRevision,
			BillOrgAndUser, UsageScopeNormal)
		fact.ValidationCode = "stale_key_revision"
		return
	}

	// --- All checks pass — valid ---
	if rec.SeatID.Valid {
		fact.CompletionSource = "exact"
		fact.QualityStatus = QualityExact
	} else {
		fact.CompletionSource = "control_event"
		fact.QualityStatus = QualityCompletedFromControlEvent
	}
	fact.AnomalyType = AnomalyNone
	fact.BillingScope = BillOrgAndUser
	fact.UserUsageScope = UsageScopeNormal
}

// markAnomaly sets all anomaly-related fields on the fact in one place.
func (e *Enricher) markAnomaly(fact *DWDFact, source string, quality QualityStatus,
	anomaly AnomalyType, reason string, billing BillingScope, usage UserUsageScope) {
	fact.CompletionSource = source
	fact.QualityStatus = quality
	fact.AnomalyType = anomaly
	fact.AnomalyReason = reason
	fact.BillingScope = billing
	fact.UserUsageScope = usage
}

func nullInt64Val(n sql.NullInt64) int64 {
	if n.Valid {
		return n.Int64
	}
	return 0
}

func nullStrPtr(n sql.NullString) *string {
	if n.Valid {
		return &n.String
	}
	return nil
}

func nullInt32Ptr(n sql.NullInt32) *int {
	if n.Valid {
		v := int(n.Int32)
		return &v
	}
	return nil
}
