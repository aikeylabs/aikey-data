package usage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

func insertUnpriced(t *testing.T, db *shared.DB, model, provider, status string, eventCount int64) {
	t.Helper()
	_, err := db.DB.Exec(
		`INSERT INTO unpriced_models (model, provider, first_seen_at, last_seen_at, event_count, status) VALUES (?, ?, ?, ?, ?, ?)`,
		model, provider, noonMs, noonMs, eventCount, status)
	if err != nil {
		t.Fatalf("insertUnpriced %s/%s: %v", provider, model, err)
	}
}

func insertSnapshot(t *testing.T, db *shared.DB, id string) {
	t.Helper()
	_, err := db.DB.Exec(
		`INSERT INTO pricing_snapshots (snapshot_id, litellm_sha256, history_sha256, overrides_sha256, aikey_version, created_at, effective_from, effective_until)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
		id, "lit-"+id, "his-"+id, "ovr-"+id, "dev", noonMs, noonMs)
	if err != nil {
		t.Fatalf("insertSnapshot %s: %v", id, err)
	}
}

func TestListUnpricedModels(t *testing.T) {
	db := setupUsageTestDB(t)
	insertUnpriced(t, db, "gpt-4o-2024-08-06", "openai", "pending", 5)
	insertUnpriced(t, db, "kimi-k2.5", "kimi_code", "pending", 9)
	insertUnpriced(t, db, "old-model", "openai", "fixed", 2)
	repo := NewSQLRepository(db)

	all, err := repo.ListUnpricedModels(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("status='' must return all rows, got %d", len(all))
	}
	if all[0].Model != "kimi-k2.5" {
		t.Errorf("want highest event_count (kimi-k2.5) first, got %q", all[0].Model)
	}

	pending, err := repo.ListUnpricedModels(context.Background(), "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Errorf("want 2 pending rows, got %d", len(pending))
	}
}

func TestUpdateUnpricedModelStatus(t *testing.T) {
	db := setupUsageTestDB(t)
	insertUnpriced(t, db, "gpt-4o-2024-08-06", "openai", "pending", 5)
	repo := NewSQLRepository(db)

	if err := repo.UpdateUnpricedModelStatus(context.Background(), "openai", "gpt-4o-2024-08-06", "acknowledged"); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.ListUnpricedModels(context.Background(), "acknowledged")
	if len(got) != 1 || got[0].Status != "acknowledged" {
		t.Fatalf("status not persisted, got %+v", got)
	}

	// Unknown (provider, model) must surface ErrNotFound, not a silent 200.
	err := repo.UpdateUnpricedModelStatus(context.Background(), "openai", "does-not-exist", "fixed")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound for unknown row, got %v", err)
	}
}

func TestGetEventAudit_Priced(t *testing.T) {
	db := setupUsageTestDB(t)
	insertSnapshot(t, db, "snap0001")
	insertDWD(t, db, dwdRow{
		EventID: "e-audit", OrgID: "org1", SeatID: "seatAudit",
		ProviderCode: "openai", Model: "gpt-4o-2024-08-06",
		EventTimeMs: noonMs, UsageDate: "2026-06-01",
		TotalTokens: 15, RequestCount: 1, BillableAmount: costPtr(0.0000525),
	})
	// Set the projector-written audit columns on that row (insertDWD covers
	// billable_amount/currency; the rest are set here so the shared helper
	// stays lean — only this test needs the full audit trail).
	if _, err := db.DB.Exec(`UPDATE usage_fact_dwd SET
			pricing_snapshot_id = ?, unit_prices_snapshot = ?, region = ?,
			endpoint_url = ?, credential_id = ?, billing_period = ?
		WHERE event_id = ?`,
		"snap0001", `{"input":0.0000025,"output":0.00001,"source":"litellm"}`, "",
		"https://api.openai.com/v1", "cred-openai-1", "2026-06", "e-audit"); err != nil {
		t.Fatalf("set audit columns: %v", err)
	}

	a, err := NewSQLRepository(db).GetEventAudit(context.Background(), "e-audit")
	if err != nil {
		t.Fatal(err)
	}
	if a.BillableAmount == nil || !approxEq(*a.BillableAmount, 0.0000525) {
		t.Errorf("billable_amount = %v, want 0.0000525", a.BillableAmount)
	}
	if a.Currency != "USD" || a.BillingPeriod != "2026-06" || a.EndpointURL != "https://api.openai.com/v1" || a.CredentialID != "cred-openai-1" {
		t.Errorf("audit scalars wrong: %+v", a)
	}
	if !json.Valid(a.UnitPrices) {
		t.Errorf("unit_prices_snapshot must be valid JSON object, got %s", string(a.UnitPrices))
	}
	if a.Snapshot == nil {
		t.Fatal("pricing_snapshot must be JOINed in")
	}
	if a.Snapshot.SnapshotID != "snap0001" || a.Snapshot.LitellmSHA256 != "lit-snap0001" {
		t.Errorf("snapshot JOIN wrong: %+v", a.Snapshot)
	}
	if a.Snapshot.EffectiveUntil != nil {
		t.Errorf("active snapshot must have nil effective_until, got %v", *a.Snapshot.EffectiveUntil)
	}
}

func TestGetEventAudit_UnpricedAndNotFound(t *testing.T) {
	db := setupUsageTestDB(t)
	// Unpriced event: no billable_amount, no snapshot id.
	insertDWD(t, db, dwdRow{
		EventID: "e-unpriced", OrgID: "org1", SeatID: "seatAudit",
		ProviderCode: "openai", Model: "mystery-model",
		EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1,
	})
	repo := NewSQLRepository(db)

	a, err := repo.GetEventAudit(context.Background(), "e-unpriced")
	if err != nil {
		t.Fatal(err)
	}
	if a.BillableAmount != nil {
		t.Errorf("unpriced event must have nil billable_amount, got %v", *a.BillableAmount)
	}
	if a.UnitPrices != nil {
		t.Errorf("unpriced event must have nil unit_prices_snapshot, got %s", string(a.UnitPrices))
	}
	if a.Snapshot != nil {
		t.Errorf("no snapshot id → no JOIN row, got %+v", a.Snapshot)
	}

	if _, err := repo.GetEventAudit(context.Background(), "no-such-event"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound for unknown event, got %v", err)
	}
}
