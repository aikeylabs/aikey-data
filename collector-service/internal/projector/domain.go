// Package projector handles ODS → DWD projection with control-plane enrichment.
package projector

import (
	"database/sql"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// QualityStatus indicates data completeness of a DWD fact.
type QualityStatus string

const (
	QualityExact                    QualityStatus = "exact"
	QualityCompletedFromControlEvent QualityStatus = "completed_from_control_event"
	QualityPartial                  QualityStatus = "partial"
	QualityInvalid                  QualityStatus = "invalid"
)

// AnomalyType classifies anomalies (D8: MVP only valid / late_report / pending_review).
type AnomalyType string

const (
	AnomalyNone                 AnomalyType = ""
	AnomalyLateReportAbnormal   AnomalyType = "late_report_abnormal_charge"
	AnomalyPendingReview        AnomalyType = "pending_review"
)

// BillingScope determines who is billed.
type BillingScope string

const (
	BillOrgAndUser BillingScope = "org_and_user"
	BillOrgOnly    BillingScope = "org_only"
	BillHoldReview BillingScope = "hold_for_review"
)

// UserUsageScope determines if the event appears in user stats.
type UserUsageScope string

const (
	UsageScopeNormal   UserUsageScope = "normal"
	UsageScopeExcluded UserUsageScope = "excluded"
	UsageScopeAbnormal UserUsageScope = "abnormal"
)

// ODSRecord represents a row read from usage_event_ods for projection.
type ODSRecord struct {
	OdsID                      int64
	EventID                    string
	EventTime                  aikeytime.Millis
	OccurredAt                 aikeytime.Millis

	OrgID                      string
	AccountID                  sql.NullString
	SeatID                     sql.NullString
	AccountStatusSnapshot      sql.NullString

	VirtualKeyID               sql.NullString
	VirtualKeyRevision         sql.NullString
	VirtualKeyHash             sql.NullString
	BindingID                  sql.NullString
	CredentialID               sql.NullString
	CredentialRevision         sql.NullString
	RealKeyHash                sql.NullString
	CredentialFingerprint      sql.NullString
	ProviderAccountFingerprint sql.NullString

	ProviderID                 sql.NullString
	ProviderCode               sql.NullString
	ProtocolType               sql.NullString
	RouteSource                sql.NullString

	Model                      sql.NullString
	RequestCount               int
	InputTokens                sql.NullInt64
	OutputTokens               sql.NullInt64
	CachedInputTokens          sql.NullInt64 // = Anthropic cache_read_input_tokens
	CacheCreationInputTokens   sql.NullInt64 // Anthropic cache_creation_input_tokens (1.25× billing)
	ReasoningTokens            sql.NullInt64
	TotalTokens                sql.NullInt64
	BillableAmount             sql.NullString
	Currency                   sql.NullString

	RequestStatus              string
	HTTPStatusCode             sql.NullInt32
	UpstreamRequestID          sql.NullString

	DwdRetryCount              int
}

// ControlEvent is a read-only projection of managed_key_control_events.
type ControlEvent struct {
	EventID            string
	OrgID              string
	AccountID          sql.NullString
	ChangeType         string
	EntityType         string
	SeatID             string
	VirtualKeyID       string
	VirtualKeyRevision string
	BindingID          sql.NullString
	CredentialID       string
	CredentialRevision string
	Revision           string // event-level monotonic counter
	ProviderID         string
	EffectiveFrom      aikeytime.Millis
	EffectiveTo        *aikeytime.Millis
	AfterSnapshotJSON  []byte
}

// DWDFact is a row to be written to usage_fact_dwd.
type DWDFact struct {
	EventID                    string
	OdsID                      int64
	OccurredAt                 aikeytime.Millis
	EventTime                  aikeytime.Millis
	UsageDate                  string // ISO date "YYYY-MM-DD" in UTC

	OrgID                      string
	AccountID                  string
	SeatID                     string

	VirtualKeyID               string
	VirtualKeyRevision         string
	VirtualKeyAlias            string
	VirtualKeyHash             string

	BindingID                  string
	BindingAlias               string

	CredentialID               string
	CredentialRevision         string
	RealKeyHash                string
	CredentialFingerprint      string
	ProviderAccountFingerprint string

	ProviderID                 string
	ProviderCode               string
	ProviderDisplayName        string
	ProtocolType               string

	RouteSource                string
	Model                      string

	RequestCount               int
	InputTokens                int64
	OutputTokens               int64
	CachedInputTokens          int64 // = Anthropic cache_read_input_tokens
	CacheCreationInputTokens   int64 // Anthropic cache_creation_input_tokens
	ReasoningTokens            int64
	TotalTokens                int64
	BillableAmount             *string
	Currency                   string

	RequestStatus              string
	HTTPStatusCode             *int
	UpstreamRequestID          string

	CompletionSource           string
	QualityStatus              QualityStatus
	ValidationCode             string
	ValidationMessage          string
	AnomalyType                AnomalyType
	AnomalyReason              string
	BillingScope               BillingScope
	UserUsageScope             UserUsageScope

	ControlEventID             string
	ControlEventRevision       string

	ProjectorVersion           string
}
