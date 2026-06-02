package usage

import (
	"encoding/json"
	"errors"
)

// ErrNotFound is returned by admin repository methods when the addressed
// row (an unpriced model, or an event audit) does not exist, so the
// handler layer can map it to HTTP 404 instead of a generic 500.
var ErrNotFound = errors.New("not found")

// UnpricedModel is one row of the pending-pricing queue (the
// `unpriced_models` table the projector UPSERTs on a price-table miss).
// Surfaced to the admin "Pending Pricing" dashboard (Stage 6) so a
// maintainer can see which (model, provider) pairs need a price added.
//
// Status lifecycle: "pending" (default, projector-set) → "acknowledged"
// (a maintainer has seen it) → "fixed" (a price was added upstream).
type UnpricedModel struct {
	Model       string `json:"model"`
	Provider    string `json:"provider"`
	FirstSeenAt int64  `json:"first_seen_at"` // unix ms
	LastSeenAt  int64  `json:"last_seen_at"`  // unix ms
	EventCount  int64  `json:"event_count"`
	Status      string `json:"status"`
	Notes       string `json:"notes,omitempty"`
}

// PricingSnapshot is the content-addressed fingerprint of the three
// pricing source files (litellm + history + overrides) that were in force
// for an event. One row per distinct file state; never deleted. A nil
// EffectiveUntil marks the currently-active snapshot.
type PricingSnapshot struct {
	SnapshotID      string `json:"snapshot_id"`
	LitellmSHA256   string `json:"litellm_sha256"`
	HistorySHA256   string `json:"history_sha256"`
	OverridesSHA256 string `json:"overrides_sha256"`
	AikeyVersion    string `json:"aikey_version,omitempty"`
	CreatedAt       int64  `json:"created_at"`     // unix ms
	EffectiveFrom   int64  `json:"effective_from"` // unix ms
	EffectiveUntil  *int64 `json:"effective_until"`
}

// EventAudit is the full cost-audit trail for a single usage event,
// assembled by JOINing usage_fact_dwd with pricing_snapshots. It is the
// payload of GET /v1/admin/events/:event_id/audit and is designed so an
// auditor can reconstruct exactly how an event was costed WITHOUT any
// external tool: UnitPrices are the literal per-token rates multiplied,
// and Snapshot pins which global price-file state (3 sha256s) was active.
//
// BillableAmount is a pointer so an unpriced event (price-table miss)
// shows JSON null rather than a misleading 0. UnitPrices is the raw
// stored JSON re-emitted as a nested object (not a string-in-string);
// nil when the event was unpriced. Snapshot is nil only for legacy events
// whose pricing_snapshot_id predates the snapshots table or didn't match.
type EventAudit struct {
	EventID           string           `json:"event_id"`
	Model             string           `json:"model"`
	ProviderCode      string           `json:"provider_code"`
	BillableAmount    *float64         `json:"billable_amount"`
	Currency          string           `json:"currency,omitempty"`
	BillingPeriod     string           `json:"billing_period,omitempty"`
	Region            string           `json:"region,omitempty"`
	EndpointURL       string           `json:"endpoint_url,omitempty"`
	CredentialID      string           `json:"credential_id,omitempty"`
	PricingSnapshotID string           `json:"pricing_snapshot_id,omitempty"`
	UnitPrices        json.RawMessage  `json:"unit_prices_snapshot"`
	Snapshot          *PricingSnapshot `json:"pricing_snapshot,omitempty"`
}
