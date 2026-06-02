package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/baseline"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
	_ "modernc.org/sqlite"
)

// Router-level integration test (cost-pricing Stage 3): drives the cost
// endpoints + admin endpoints through the REAL api.NewRouter — including
// the ServiceTokenAuth middleware and Go 1.22 path-param routing — against
// the REAL baseline+rc.8 schema. This is the self-closing HTTP E2E for
// Stage 3: it proves routes are registered, auth is enforced, path params
// flow, and JSON serialization carries the new cost / audit fields.
//
// (A deployed curl through aikey-control is deferred: in Personal/Trial the
// query handler is mounted as control-service's UsageFacade behind user-JWT
// auth, and /v1/admin/* web forwarding is Stage 6 work — so the faithful
// pre-Stage-6 E2E is this in-process router test.)

const intToken = "integration-test-token"

func setupRouterDB(t *testing.T) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, stmt := range baseline.DDLFor(baseline.ComponentData, baseline.DialectSQLite) {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("baseline DDL: %v", err)
		}
	}
	// Post-baseline migrations (kept in lock-step with the canonical list in
	// usage/repository_sql_test.go::setupUsageTestDB — rc.5 / rc.6 / rc.8).
	post := []string{
		`ALTER TABLE usage_event_ods ADD COLUMN app_slug TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN app_slug TEXT`,
		`ALTER TABLE usage_event_ods ADD COLUMN session_id TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN session_id TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN billing_period TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN unit_prices_snapshot TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN pricing_snapshot_id TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN region TEXT`,
		`ALTER TABLE usage_fact_dwd  ADD COLUMN endpoint_url TEXT`,
		`CREATE TABLE IF NOT EXISTS unpriced_models (
			model TEXT NOT NULL, provider TEXT NOT NULL,
			first_seen_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL,
			event_count INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending', notes TEXT,
			PRIMARY KEY (model, provider))`,
		`CREATE TABLE IF NOT EXISTS pricing_snapshots (
			snapshot_id TEXT NOT NULL, litellm_sha256 TEXT NOT NULL,
			history_sha256 TEXT NOT NULL, overrides_sha256 TEXT NOT NULL,
			aikey_version TEXT, created_at INTEGER NOT NULL,
			effective_from INTEGER NOT NULL, effective_until INTEGER, notes TEXT,
			PRIMARY KEY (snapshot_id))`,
	}
	for _, stmt := range post {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("post-baseline DDL %q: %v", stmt, err)
		}
	}
	return raw
}

func TestRouter_CostAndAdmin_Integration(t *testing.T) {
	raw := setupRouterDB(t)
	ms := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UnixMilli()

	// One priced anthropic event with a full audit trail.
	if _, err := raw.Exec(`INSERT INTO usage_fact_dwd (
		event_id, ods_id, occurred_at, event_time, usage_date,
		org_id, seat_id, provider_code, model, request_count, total_tokens,
		request_status, completion_source, quality_status, billing_scope,
		user_usage_scope, projector_version,
		billable_amount, currency, credential_id, region, endpoint_url,
		billing_period, pricing_snapshot_id, unit_prices_snapshot
	) VALUES ('evt-int', 9001, ?, ?, '2026-06-01',
		'org1', 'seatINT', 'anthropic', 'claude-x', 1, 100,
		'success','test','ok','user_only','normal','test',
		0.005, 'USD', 'cred-1', '', 'https://api.anthropic.com',
		'2026-06', 'snapINT', '{"input":0.000003,"output":0.000015,"source":"litellm"}')`,
		ms, ms); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO pricing_snapshots (snapshot_id, litellm_sha256, history_sha256, overrides_sha256, aikey_version, created_at, effective_from, effective_until)
		VALUES ('snapINT','lit','his','ovr','dev', ?, ?, NULL)`, ms, ms); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO unpriced_models (model, provider, first_seen_at, last_seen_at, event_count, status)
		VALUES ('gpt-4o-2024-08-06','openai', ?, ?, 3, 'pending')`, ms, ms); err != nil {
		t.Fatal(err)
	}

	repo := usage.NewSQLRepository(shared.NewDB(raw, shared.DialectSQLite))
	router := NewRouter(NewUsageHandler(repo), NewAdminHandler(repo), raw, intToken)

	do := func(method, path string, withAuth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if withAuth {
			req.Header.Set("Authorization", "Bearer "+intToken)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 1. Auth enforced: no token → 401.
	if w := do("GET", "/v1/usage/personal/by-protocol/total?seat_id=seatINT", false); w.Code != http.StatusUnauthorized {
		t.Errorf("no-token want 401, got %d", w.Code)
	}

	// 2. Cost field flows through by-protocol/total.
	w := do("GET", "/v1/usage/personal/by-protocol/total?seat_id=seatINT", true)
	if w.Code != 200 {
		t.Fatalf("by-protocol/total want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var totals []usage.ProtocolTotal
	if err := json.NewDecoder(w.Body).Decode(&totals); err != nil {
		t.Fatal(err)
	}
	if len(totals) != 1 || totals[0].ProtocolType != "anthropic" {
		t.Fatalf("unexpected totals: %+v", totals)
	}
	if !approxEqInt(totals[0].CostUSD, 0.005) || totals[0].PricedRequestCount != 1 {
		t.Errorf("cost trio wrong: cost=%v priced=%d", totals[0].CostUSD, totals[0].PricedRequestCount)
	}

	// 3. Admin unpriced-models list.
	w = do("GET", "/v1/admin/unpriced-models?status=pending", true)
	if w.Code != 200 {
		t.Fatalf("unpriced-models want 200, got %d", w.Code)
	}
	var um struct {
		Models []usage.UnpricedModel `json:"models"`
	}
	if err := json.NewDecoder(w.Body).Decode(&um); err != nil {
		t.Fatal(err)
	}
	if len(um.Models) != 1 || um.Models[0].Model != "gpt-4o-2024-08-06" {
		t.Errorf("unexpected unpriced models: %+v", um.Models)
	}

	// 4. Admin status update via path params → then audit endpoint.
	if w := do("POST", "/v1/admin/unpriced-models/openai/gpt-4o-2024-08-06?status=acknowledged", true); w.Code != 200 {
		t.Errorf("status update want 200, got %d (%s)", w.Code, w.Body.String())
	}

	// 5. Admin event audit (path param + JOINed snapshot).
	w = do("GET", "/v1/admin/events/evt-int/audit", true)
	if w.Code != 200 {
		t.Fatalf("audit want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var audit usage.EventAudit
	if err := json.NewDecoder(w.Body).Decode(&audit); err != nil {
		t.Fatal(err)
	}
	if audit.EventID != "evt-int" || audit.BillableAmount == nil || audit.Snapshot == nil {
		t.Errorf("audit incomplete: %+v", audit)
	}
	if audit.Snapshot != nil && audit.Snapshot.SnapshotID != "snapINT" {
		t.Errorf("audit snapshot JOIN wrong: %+v", audit.Snapshot)
	}
}

func approxEqInt(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
