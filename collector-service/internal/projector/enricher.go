package projector

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
)

// Virtual-key prefixes that originate from the user's local vault and
// thus deliberately do NOT have a corresponding row in
// managed_key_control_events:
//
//   - "personal:<alias>"            aikey-cli adds a personal-route VK
//   - "oauth:<session_id>"          aikey-proxy issues per-OAuth-session VK
//                                   (e.g. claude / codex login flows)
//   - "app:<app_slug>"              aikey-cli registers an app and proxy
//                                   issues "app:<slug>" VK (2026-05 phase 4)
//   - "probe:<alias_name>"          aikey-proxy probe pipeline (Mode C,
//                                   `/probe/<alias>/v1/...`), URL-explicit
//                                   alias dereference; vault-resolved at
//                                   request time, no managed-key control
//                                   row (2026-05 credential mode spec §1.1.C)
//
// Looking these up would (a) be 100% a miss in Trial / Production where
// the table exists but is empty for these VK families, or (b) error out
// with "no such table" in Personal edition where the Control component
// is not installed — which used to stall the projector in retry loop
// (see bugfix 20260522-projector-stuck-mark-dead-letter-param-order.md
// for the cascade that exposed this gap).
const (
	personalVKPrefix = "personal:"
	oauthVKPrefix    = "oauth:"
	appVKPrefix      = "app:"
	// probe: added BR-rc.5-54 (2026-05-25). Mode C probe events were
	// landing in ODS with "probe:<alias>" VKs since the probe pipeline
	// shipped, but this list still only covered the 3 pre-probe families
	// → projector ENRICH_FAILED → 67 stale probe rows stuck in retry on
	// Personal edition → /user/apps/<slug> dashboard never saw probe
	// traffic in DWD. Same short-circuit rationale as oauth: / app:.
	probeVKPrefix = "probe:"
)

// vaultOriginVKPrefixes lists all VK prefixes that originate from the
// user's local vault (any edition). Adding a new vault-origin VK family
// requires extending this list AND adding a corresponding enricher test
// — otherwise the projector will enter the retry loop the new family.
var vaultOriginVKPrefixes = []string{personalVKPrefix, oauthVKPrefix, appVKPrefix, probeVKPrefix}

func isVaultOriginVK(vk string) bool {
	for _, p := range vaultOriginVKPrefixes {
		if strings.HasPrefix(vk, p) {
			return true
		}
	}
	return false
}

const projectorVersion = "0.1.0"

// Enricher transforms an ODS record into a DWD fact by looking up control events
// and computing cost from the pricing resolver.
type Enricher struct {
	controlReader ControlEventReader
	resolver      *pricing.Resolver // cost-pricing audit (v1.0.0-rc.8); may be nil
	unpriced      UnpricedSink      // optional; set via SetUnpricedSink (Stage 2.4)
}

// NewEnricher creates an enricher that reads control events for enrichment (D5)
// and computes cost via the pricing resolver (rc.8). resolver may be nil — cost
// is then left NULL — so callers/tests without pricing still work.
func NewEnricher(cr ControlEventReader, resolver *pricing.Resolver) *Enricher {
	return &Enricher{controlReader: cr, resolver: resolver}
}

// SetUnpricedSink wires the async queue that records pricing misses. Optional —
// when unset (tests), a miss only WARN-logs. Kept a setter rather than a
// NewEnricher param so that signature (and its many test callers) stays stable.
func (e *Enricher) SetUnpricedSink(s UnpricedSink) { e.unpriced = s }

// Enrich takes an ODS record, looks up the matching control event, and produces a DWD fact.
func (e *Enricher) Enrich(ctx context.Context, rec *ODSRecord) (*DWDFact, error) {
	fact := e.buildBaseFact(rec)

	// Cost is independent of ownership/anomaly classification: any event with a
	// model + tokens has a cost regardless of how the control-event lookup turns
	// out. Compute it once here so every return path below carries the cost +
	// audit trail.
	e.applyCost(fact)

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

	// Vault-origin VK short-circuit (2026-05-13 binary-isolation refactor
	// extended 2026-05-22 to cover oauth: + app: prefixes per
	// bugfix 20260522-projector-stuck-mark-dead-letter-param-order.md).
	//
	// All VKs minted from the user's local vault (personal: / oauth: /
	// app:) deliberately have no managed_key_control_events row. In
	// Personal edition (control component not installed) querying the
	// table raises "no such table" and stalls the projector in a retry
	// loop. In Trial / Production it always returns nil (table empty
	// for these VK families) and falls through to no_control_event with
	// the same observable shape — short-circuiting here makes the intent
	// explicit and saves a DB round-trip.
	//
	// UserUsageScope=normal so the row surfaces on /user/overview
	// (Personal queries don't filter on scope, but explicit is clearer).
	if isVaultOriginVK(rec.VirtualKeyID.String) {
		// CompletionSource stays "personal_vault_key" verbatim (not
		// renamed) so DWD enum stays back-compatible with 5/13 ~ 5/22
		// rows already in usage_fact_dwd. The label is now a
		// historical artifact — the actual classifier is the prefix
		// list above (personal: / oauth: / app:).
		fact.CompletionSource = "personal_vault_key"
		fact.QualityStatus = QualityPartial
		fact.BillingScope = BillOrgOnly
		fact.UserUsageScope = UsageScopeNormal
		fact.AnomalyType = AnomalyNone
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
		CacheCreationInputTokens:   nullInt64Val(rec.CacheCreationInputTokens),
		ReasoningTokens:            nullInt64Val(rec.ReasoningTokens),
		TotalTokens:                nullInt64Val(rec.TotalTokens),
		BillableAmount:             nullStrPtr(rec.BillableAmount),
		Currency:                   rec.Currency.String,

		RequestStatus:              rec.RequestStatus,
		HTTPStatusCode:             nullInt32Ptr(rec.HTTPStatusCode),
		UpstreamRequestID:          rec.UpstreamRequestID.String,

		ProjectorVersion:           projectorVersion,

		// Phase 4 Connected Apps (v1.0.0-rc.5): copy from ODS verbatim.
		AppSlug:                    rec.AppSlug.String,

		// Performance session dimension (v1.0.0-rc.6): copy verbatim
		// from ODS — no enrichment or fallback logic. The single
		// source of truth for what "session" means is what the proxy
		// extracted via sessionid/fingerprint.yaml at request time.
		SessionID:                  rec.SessionID.String,

		// Cost-pricing audit (v1.0.0-rc.8): region/endpoint passthrough from ODS.
		Region:                     rec.Region.String,
		EndpointURL:                rec.EndpointURL.String,
	}
}

// applyCost computes the event's cost and stamps the audit trail. It runs for
// EVERY event (independent of the ownership/anomaly checks below). On an unknown
// model it leaves BillableAmount NULL and WARN-logs — never a zero cost
// (CLAUDE.md no-lazy-defaults). The unpriced_models enqueue + pricing_snapshots
// UPSERT land in Stage 2.4; here we record the active snapshot id and the
// per-event unit prices.
func (e *Enricher) applyCost(fact *DWDFact) {
	// Monthly billing bucket from the event's occurred time, derived once and
	// stored so account/forecast queries match a string index instead of
	// re-deriving from a timestamp per row (cross-dialect-safe).
	fact.BillingPeriod = fact.EventTime.Time().Format("2006-01")

	if e.resolver == nil {
		return // no pricing configured (e.g. unit tests) → leave cost NULL
	}
	// Record which global pricing state was active even on a miss, so an
	// unpriced row still pins "this is the price-file set that had no entry".
	fact.PricingSnapshotID = e.resolver.Snapshot().SnapshotID

	if fact.Model == "" {
		return // no model → cannot price
	}
	// 方案 A: fact.InputTokens is now PURE (uncached). The long-context price tier
	// (e.g. Anthropic >128k) keys off the TOTAL context window, so pass pure + both
	// cache buckets — otherwise a cache-heavy request whose total exceeds 128k but
	// whose pure input is small would wrongly get the cheaper base-tier rate.
	totalContext := fact.InputTokens + fact.CachedInputTokens + fact.CacheCreationInputTokens
	up, err := e.resolver.Lookup(fact.ProviderCode, fact.Model,
		fact.EventTime.Time().UnixMilli(), totalContext, fact.OrgID)
	if err != nil {
		// Unknown (provider, model): cost stays NULL (never zero); surface
		// loudly AND enqueue for the "pending pricing" dashboard. The enqueue is
		// async + non-blocking — it must never stall the projector main path.
		if e.unpriced != nil {
			e.unpriced.Enqueue(fact.ProviderCode, fact.Model)
		}
		slog.Warn("pricing miss",
			"event.name", "projector.pricing.miss",
			"provider", fact.ProviderCode, "model", fact.Model)
		return
	}
	cost := pricing.ComputeCost(*up, pricing.TokenCounts{
		Input:         fact.InputTokens,
		Output:        fact.OutputTokens,
		CacheRead:     fact.CachedInputTokens,
		CacheCreation: fact.CacheCreationInputTokens,
		Reasoning:     fact.ReasoningTokens,
	}, inputIncludesCache(fact.ProviderCode))

	costStr := pricing.FormatCostUSD(cost)
	fact.BillableAmount = &costStr
	fact.Currency = "USD"
	if b, mErr := json.Marshal(up); mErr == nil {
		s := string(b)
		fact.UnitPricesSnapshot = &s
	}
}

// inputIncludesCache reports whether the proxy-reported Input token count INCLUDES
// the cache_read + cache_creation buckets — i.e. whether ComputeCost must subtract
// them to get the uncached portion billed at the input rate.
//
// 方案 A (2026-06-04): always FALSE. The proxy adapters now report PURE (uncached)
// input — cache lives in its own DWD columns (cached_input_tokens /
// cache_creation_input_tokens), and the long-context tier uses the reconstructed
// total (enricher.go totalContext). So ComputeCost must NOT subtract again.
//
// History: fix B briefly returned true to compensate for the adapter pre-summing
// cache into the reported input (the cache over-charge stopgap). 方案 A moved that
// subtraction to the source (the adapter reports pure), so the collector reverts to
// false — the value cost.go's `false` branch documents as "Input already uncached".
// REQUIRES the pure-input proxy build to be deployed first (see Phase 5 deploy
// order). See roadmap20260320/技术实现/update/20260604-token-input-纯输入语义治本-方案A.md
// and bugfix/2026-06-04-quota-token-metric-cache-semantics.md.
func inputIncludesCache(provider string) bool {
	return false
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
