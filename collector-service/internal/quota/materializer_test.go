package quota

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	// Side-effect: registers ComponentControl migrations (incl. rc7 quota tables).
	_ "github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions_master"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	_ "modernc.org/sqlite"
)

// freshDB applies the REAL ComponentControl (quota_subject/quota_counter) AND
// ComponentData (usage_fact_dwd) migration chains, so the materializer is tested
// against production DDL — never an inline mock. ComponentData is required
// because the materializer now recomputes used from SUM(usage_fact_dwd).
func freshDB(t *testing.T) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := versions.UpgradeComponentsTo(context.Background(), raw, dbmigrate.DialectSQLite,
		[]dbmigrate.Component{dbmigrate.ComponentControl, dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate Control+Data: %v", err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

func insertSubject(t *testing.T, db *shared.DB, id, kind, members, rules string) {
	t.Helper()
	var mem any
	if members != "" {
		mem = members
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO quota_subject (subject_id, subject_kind, display_name, members, rules,
			source_system, external_ref, enabled, created_at, updated_at)
		VALUES (?, ?, '', ?, ?, 'aikey_seat', ?, 1, 0, 0)`,
		id, kind, mem, rules, id)
	if err != nil {
		t.Fatalf("insert subject %s: %v", id, err)
	}
}

// insertDWDFact writes one usage_fact_dwd row (real schema). tokens lands in
// input_tokens (the SUM covers input+output+cached+creation+reasoning). Only the
// NOT-NULL-without-default columns + the ones the quota SUM reads are set; the
// rest take their schema defaults. eventID must be unique per call.
var dwdODSSeq int64 // monotonic ods_id source (usage_fact_dwd.ods_id is UNIQUE)

func insertDWDFact(t *testing.T, db *shared.DB, eventID, seatID, billingPeriod string, tokens int64, billable float64) {
	t.Helper()
	dwdODSSeq++
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO usage_fact_dwd (
			event_id, ods_id, occurred_at, event_time, usage_date,
			seat_id, billing_period, input_tokens, billable_amount,
			request_count, request_status, completion_source, quality_status,
			billing_scope, user_usage_scope, projector_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'ok', 'upstream', 'ok', 'billable', 'team', 1)`,
		eventID, dwdODSSeq, 0, 0, "2026-06-03", seatID, billingPeriod, tokens, billable)
	if err != nil {
		t.Fatalf("insert dwd fact %s: %v", eventID, err)
	}
}

func readCounter(t *testing.T, db *shared.DB, subjectID, metric, pk string) (float64, []int) {
	t.Helper()
	var (
		used float64
		tt   sql.NullString
	)
	err := db.QueryRowContext(context.Background(),
		`SELECT used_amount, triggered_thresholds FROM quota_counter
		 WHERE subject_id = ? AND metric = ? AND period_key = ?`, subjectID, metric, pk).Scan(&used, &tt)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	var trig []int
	if tt.Valid && tt.String != "" {
		_ = json.Unmarshal([]byte(tt.String), &trig)
	}
	return used, trig
}

func TestMaterializer_RecomputeAndThresholds(t *testing.T) {
	db := freshDB(t)
	// seat with a monthly token rule: limit 100, thresholds 80% (warn) + 100% (block).
	insertSubject(t, db, "seat-a", "seat", "",
		`[{"metric":"tokens","period":"monthly","limit_amount":100,"thresholds":[{"pct":80,"action":"warn"},{"pct":100,"action":"hard_block"}]}]`)

	m := NewMaterializer(NewStorage(db), time.Hour)
	ctx := context.Background()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	pk := "monthly:2026-06"

	// DWD has 90 tokens for the period → recompute used 90, crosses 80% (not 100%).
	insertDWDFact(t, db, "e1", "seat-a", "2026-06", 90, 0)
	m.OnFact(ctx, "seat-a", 90, 0, "2026-06", now)
	used, trig := readCounter(t, db, "seat-a", MetricTokens, pk)
	if used != 90 {
		t.Fatalf("after 90: want used 90 got %v", used)
	}
	if len(trig) != 1 || trig[0] != 80 {
		t.Errorf("after 90: want triggered [80] got %v", trig)
	}

	// +20 more in DWD → recompute used 110, crosses 100% too; 80 not re-recorded.
	insertDWDFact(t, db, "e2", "seat-a", "2026-06", 20, 0)
	m.OnFact(ctx, "seat-a", 20, 0, "2026-06", now)
	used, trig = readCounter(t, db, "seat-a", MetricTokens, pk)
	if used != 110 {
		t.Fatalf("after +20: want used 110 got %v", used)
	}
	if len(trig) != 2 || trig[0] != 80 || trig[1] != 100 {
		t.Errorf("after +20: want triggered [80 100] got %v", trig)
	}

	// A different month's usage must NOT leak into this period's counter.
	insertDWDFact(t, db, "e3", "seat-a", "2026-05", 5000, 0)
	m.OnFact(ctx, "seat-a", 5000, 0, "2026-05", now)
	if u, _ := readCounter(t, db, "seat-a", MetricTokens, pk); u != 110 {
		t.Errorf("period isolation: 2026-06 counter must stay 110, got %v", u)
	}

	// A seat with no quota → no counter row.
	m.OnFact(ctx, "seat-none", 5000, 0, "2026-06", now)
	if u, _ := readCounter(t, db, "seat-none", MetricTokens, pk); u != 0 {
		t.Errorf("no-quota seat must not materialize, got %v", u)
	}
}

func TestMaterializer_RestartSafeNoUndercount(t *testing.T) {
	// Regression for the forward-only undercount: a fresh materializer (≈ collector
	// restart) over a period that already has DWD facts must recompute the FULL
	// total on the very next fact, not just the delta it personally observed.
	db := freshDB(t)
	insertSubject(t, db, "seat-a", "seat", "",
		`[{"metric":"tokens","period":"monthly","limit_amount":1000,"thresholds":[{"pct":100,"action":"hard_block"}]}]`)
	ctx := context.Background()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	pk := "monthly:2026-06"

	// 300 tokens already in DWD before this materializer instance exists.
	insertDWDFact(t, db, "old1", "seat-a", "2026-06", 200, 0)
	insertDWDFact(t, db, "old2", "seat-a", "2026-06", 100, 0)

	// Brand-new materializer (no prior in-memory tally). One new 50-token fact.
	m := NewMaterializer(NewStorage(db), time.Hour)
	insertDWDFact(t, db, "new1", "seat-a", "2026-06", 50, 0)
	m.OnFact(ctx, "seat-a", 50, 0, "2026-06", now)

	// Forward tally would show 50; recompute shows the true 350.
	if used, _ := readCounter(t, db, "seat-a", MetricTokens, pk); used != 350 {
		t.Fatalf("restart-safe recompute: want 350 (200+100+50) got %v", used)
	}
}

func TestMaterializer_GroupSharedPool(t *testing.T) {
	db := freshDB(t)
	// group eng covering seat-a + seat-b, monthly token limit 100.
	insertSubject(t, db, "grp-eng", "group", `["seat-a","seat-b"]`,
		`[{"metric":"tokens","period":"monthly","limit_amount":100,"thresholds":[{"pct":100,"action":"hard_block"}]}]`)

	m := NewMaterializer(NewStorage(db), time.Hour)
	ctx := context.Background()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	pk := "monthly:2026-06"

	// both seats' usage rolls up to the SAME group counter (shared pool).
	insertDWDFact(t, db, "a1", "seat-a", "2026-06", 60, 0)
	m.OnFact(ctx, "seat-a", 60, 0, "2026-06", now)
	insertDWDFact(t, db, "b1", "seat-b", "2026-06", 50, 0)
	m.OnFact(ctx, "seat-b", 50, 0, "2026-06", now)

	used, trig := readCounter(t, db, "grp-eng", MetricTokens, pk)
	if used != 110 {
		t.Errorf("group shared pool: want 110 (60+50) got %v", used)
	}
	if len(trig) != 1 || trig[0] != 100 {
		t.Errorf("group crossed 100%%: want [100] got %v", trig)
	}
}

func TestMaterializer_DualValueTokenAndUsd(t *testing.T) {
	// D-U2: quota_counter carries BOTH token and usd, regardless of the rule's
	// metric. A subject with a usd-ONLY rule must still materialize a tokens row,
	// so the admin can switch the rule's metric / the overview can show both.
	db := freshDB(t)
	insertSubject(t, db, "seat-a", "seat", "",
		`[{"metric":"usd","period":"monthly","limit_amount":50,"thresholds":[{"pct":100,"action":"hard_block"}]}]`)

	m := NewMaterializer(NewStorage(db), time.Hour)
	ctx := context.Background()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	pk := "monthly:2026-06"

	// one priced fact: 100 tokens, $0.5 billable.
	insertDWDFact(t, db, "e1", "seat-a", "2026-06", 100, 0.5)
	m.OnFact(ctx, "seat-a", 100, 0.5, "2026-06", now)

	// usd row (rule metric): recomputed + threshold-checked (0.5 < 50 → no trigger).
	if used, trig := readCounter(t, db, "seat-a", MetricUSD, pk); used != 0.5 || len(trig) != 0 {
		t.Fatalf("usd row: want 0.5 / [] got %v / %v", used, trig)
	}
	// tokens row (NOT a rule metric) must still be materialized for display.
	if used, _ := readCounter(t, db, "seat-a", MetricTokens, pk); used != 100 {
		t.Fatalf("dual-value: tokens row must materialize even with usd-only rule, want 100 got %v", used)
	}

	// An unpriced fact (billable=0) moves only tokens; the usd row keeps its value.
	insertDWDFact(t, db, "e2", "seat-a", "2026-06", 40, 0)
	m.OnFact(ctx, "seat-a", 40, 0, "2026-06", now)
	if used, _ := readCounter(t, db, "seat-a", MetricTokens, pk); used != 140 {
		t.Fatalf("tokens after unpriced fact: want 140 got %v", used)
	}
	if used, _ := readCounter(t, db, "seat-a", MetricUSD, pk); used != 0.5 {
		t.Fatalf("usd unchanged by unpriced fact: want 0.5 got %v", used)
	}
}

// TestMaterializer_HardBlockCrossing_BumpsSyncVersion is the P5 regression guard:
// a newly-crossed hard_block threshold bumps the seat's account sync_version (so
// the proxy re-pulls the over-limit baseline promptly), while a warn-only crossing
// does NOT. Uses the real master schema (global_accounts → organizations →
// org_seats → account_sync_versions) so the seat→account resolve + bump UPSERT run
// against production DDL.
func TestMaterializer_HardBlockCrossing_BumpsSyncVersion(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	mustExecQ := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("setup exec failed (%s): %v", q, err)
		}
	}
	// FK chains for two seats: acc-1 (hard_block) + acc-2 (warn-only).
	mustExecQ(`INSERT INTO global_accounts (account_id, email) VALUES (?, ?)`, "acc-1", "a1@x.z")
	mustExecQ(`INSERT INTO global_accounts (account_id, email) VALUES (?, ?)`, "acc-2", "a2@x.z")
	mustExecQ(`INSERT INTO organizations (org_id, name) VALUES (?, ?)`, "org-1", "Org")
	mustExecQ(`INSERT INTO org_seats (seat_id, org_id, account_id, invited_email, seat_status) VALUES (?,?,?,?,?)`,
		"seat-hb", "org-1", "acc-1", "a1@x.z", "claimed")
	mustExecQ(`INSERT INTO org_seats (seat_id, org_id, account_id, invited_email, seat_status) VALUES (?,?,?,?,?)`,
		"seat-warn", "org-1", "acc-2", "a2@x.z", "claimed")

	insertSubject(t, db, "seat-hb", "seat", "",
		`[{"metric":"usd","period":"monthly","limit_amount":0.001,"thresholds":[{"pct":100,"action":"hard_block"}]}]`)
	insertSubject(t, db, "seat-warn", "seat", "",
		`[{"metric":"usd","period":"monthly","limit_amount":0.001,"thresholds":[{"pct":80,"action":"warn"}]}]`)

	m := NewMaterializer(NewStorage(db), time.Hour)
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	// hard_block seat crosses (billable 0.5 >> limit 0.001) → must bump acc-1.
	insertDWDFact(t, db, "hb1", "seat-hb", "2026-06", 100, 0.5)
	m.OnFact(ctx, "seat-hb", 100, 0.5, "2026-06", now)
	var v int64
	if err := db.QueryRowContext(ctx, `SELECT sync_version FROM account_sync_versions WHERE account_id=?`, "acc-1").Scan(&v); err != nil {
		t.Fatalf("hard_block crossing must bump acc-1 sync_version: %v", err)
	}
	if v < 1 {
		t.Errorf("acc-1 sync_version want >=1 after hard_block, got %d", v)
	}

	// warn-only seat crosses the 80% warn (0.5 >= 0.0008) but action=warn → NO bump.
	insertDWDFact(t, db, "wn1", "seat-warn", "2026-06", 100, 0.5)
	m.OnFact(ctx, "seat-warn", 100, 0.5, "2026-06", now)
	var cnt int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_sync_versions WHERE account_id=?`, "acc-2").Scan(&cnt); err != nil {
		t.Fatalf("count acc-2: %v", err)
	}
	if cnt != 0 {
		t.Errorf("warn-only crossing must NOT bump (acc-2 rows want 0, got %d)", cnt)
	}
}
