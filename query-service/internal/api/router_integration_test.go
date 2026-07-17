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
	// Seed timestamp is RELATIVE to now (yesterday noon UTC), not a fixed
	// date: the personal endpoints default to a rolling last-30-days window
	// (QueryParams.Defaults), so a hard-coded date is a time bomb — the
	// original 2026-06-01 seed started silently failing on 2026-07-02.
	seedDay := time.Now().UTC().AddDate(0, 0, -1)
	ms := time.Date(seedDay.Year(), seedDay.Month(), seedDay.Day(), 12, 0, 0, 0, time.UTC).UnixMilli()
	seedDate := seedDay.Format("2006-01-02")

	// One priced anthropic event with a full audit trail.
	if _, err := raw.Exec(`INSERT INTO usage_fact_dwd (
		event_id, ods_id, occurred_at, event_time, usage_date,
		org_id, seat_id, provider_code, model, request_count, total_tokens,
		request_status, completion_source, quality_status, billing_scope,
		user_usage_scope, projector_version,
		billable_amount, currency, credential_id, region, endpoint_url,
		billing_period, pricing_snapshot_id, unit_prices_snapshot
	) VALUES ('evt-int', 9001, ?, ?, ?,
		'org1', 'seatINT', 'anthropic', 'claude-x', 1, 100,
		'success','test','ok','user_only','normal','test',
		0.005, 'USD', 'cred-1', '', 'https://api.anthropic.com',
		?, 'snapINT', '{"input":0.000003,"output":0.000015,"source":"litellm"}')`,
		ms, ms, seedDate, seedDate[:7]); err != nil {
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
	router := NewRouter(NewUsageHandler(repo), NewAdminHandler(repo), nil, raw, intToken)

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

// TestRouter_ByAgent_Integration is the HTTP-level E2E for the 2026-07-17
// "Usage By Agent" breakdown: it drives GET /v1/usage/personal/by-agent/total
// through the REAL api.NewRouter (ServiceTokenAuth + Go 1.22 routing) against
// the REAL baseline schema PLUS a real org_seats table, and proves the
// authorization scope end-to-end — the caller sees their own seat + the agents
// they own, and a STRANGER's agent never leaks. The repository unit test
// (usage/by_agent_test.go) covers the same scope at the SQL layer; this one
// additionally proves the route is registered, auth is enforced, seat_id flows
// as a query param, and the JSON shape serializes.
func TestRouter_ByAgent_Integration(t *testing.T) {
	raw := setupRouterDB(t)

	// org_seats is a control-plane table (not in the data-component baseline);
	// seed it by hand mirroring the real DDL — seat_type (v1.0.0 / alpha.1),
	// alias, parent_seat_id (alpha.5). Same convention as usage/by_agent_test.go.
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS org_seats (
			seat_id       TEXT NOT NULL PRIMARY KEY,
			org_id        TEXT NOT NULL,
			invited_email TEXT NOT NULL,
			seat_type     TEXT NOT NULL DEFAULT 'human',
			alias         TEXT,
			parent_seat_id TEXT)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("org_seats DDL: %v", err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO org_seats (seat_id, org_id, invited_email, seat_type, alias) VALUES ('seatINT','org1','me@corp','human','Me')`,
		`INSERT INTO org_seats (seat_id, org_id, invited_email, seat_type, alias, parent_seat_id) VALUES ('agentA','org1','a@corp','digital_employee','Agent A','seatINT')`,
		`INSERT INTO org_seats (seat_id, org_id, invited_email, seat_type, alias, parent_seat_id) VALUES ('agentX','org1','x@corp','digital_employee','Stranger Agent','seat-other')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed org_seats: %v", err)
		}
	}

	// Seed relative to now — personal endpoints default to a rolling 30-day
	// window, so a hard-coded date silently ages out (see the note in
	// TestRouter_CostAndAdmin_Integration).
	seedDay := time.Now().UTC().AddDate(0, 0, -1)
	ms := time.Date(seedDay.Year(), seedDay.Month(), seedDay.Day(), 12, 0, 0, 0, time.UTC).UnixMilli()
	seedDate := seedDay.Format("2006-01-02")
	insertSeatUsage := func(eventID, seat string, tokens, reqs int64) {
		t.Helper()
		if _, err := raw.Exec(`INSERT INTO usage_fact_dwd (
			event_id, ods_id, occurred_at, event_time, usage_date,
			org_id, seat_id, provider_code, model, request_count, total_tokens,
			request_status, completion_source, quality_status, billing_scope,
			user_usage_scope, projector_version
		) VALUES (?, ?, ?, ?, ?, 'org1', ?, 'anthropic', 'claude-x', ?, ?,
			'success','test','ok','user_only','normal','test')`,
			eventID, time.Now().UnixNano(), ms, ms, seedDate, seat, reqs, tokens); err != nil {
			t.Fatalf("seed usage for %s: %v", seat, err)
		}
	}
	insertSeatUsage("ev-me", "seatINT", 1000, 5)
	insertSeatUsage("ev-agentA", "agentA", 300, 2)
	insertSeatUsage("ev-agentX", "agentX", 9999, 40) // stranger's agent — must NOT surface

	repo := usage.NewSQLRepository(shared.NewDB(raw, shared.DialectSQLite))
	router := NewRouter(NewUsageHandler(repo), NewAdminHandler(repo), nil, raw, intToken)

	do := func(withAuth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/v1/usage/personal/by-agent/total?seat_id=seatINT", nil)
		if withAuth {
			req.Header.Set("Authorization", "Bearer "+intToken)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// Auth enforced.
	if w := do(false); w.Code != http.StatusUnauthorized {
		t.Errorf("no-token want 401, got %d", w.Code)
	}

	w := do(true)
	if w.Code != 200 {
		t.Fatalf("by-agent/total want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var rows []usage.AgentTotal
	if err := json.NewDecoder(w.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	byID := map[string]usage.AgentTotal{}
	for _, r := range rows {
		byID[r.SeatID] = r
	}
	// The point: stranger's agent must be absent through the real HTTP path.
	if _, leaked := byID["agentX"]; leaked {
		t.Fatalf("SECURITY: stranger's agent leaked via HTTP by-agent endpoint: %+v", rows)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (self + own agent), got %d: %+v", len(rows), rows)
	}
	if me := byID["seatINT"]; me.IsAgent || me.SeatAlias != "Me" || me.TotalTokens != 1000 {
		t.Errorf("own seat row wrong: %+v", me)
	}
	if a := byID["agentA"]; !a.IsAgent || a.ParentSeatID != "seatINT" || a.SeatAlias != "Agent A" || a.TotalTokens != 300 {
		t.Errorf("owned agent row wrong: %+v", a)
	}
}

func approxEqInt(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
