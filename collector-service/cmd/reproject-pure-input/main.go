// Command reproject-pure-input is the 方案 A one-time migration that converts
// pre-方案-A usage_fact_dwd rows (input_tokens = TOTAL, including cache) to the
// new PURE-input semantics, re-pricing each row with the current resolver.
//
// Why: 方案 A made the proxy report PURE input and the collector bill with
// inputIncludesCache=false. Rows projected before the cutover still carry the
// TOTAL input (cache folded in) and a billable computed under the old assumption.
// This migration brings history in line so dashboards + ledgers are consistent.
//
// Old/new marker: projector_version. Rows at "0.1.0" are old (total); this tool
// converts them and stamps "0.2.0". Rows already at "0.2.0" (projected by the
// post-方案-A collector) are skipped — so the run is idempotent and never
// double-subtracts cache.
//
// Conversion per row:
//   pure          = max(0, input_tokens - cached - cache_creation)
//   total_context = input_tokens (the old value WAS the total) → used for the
//                   128k price tier so cache-heavy long-context keeps the right rate
//   billable      = ComputeCost(resolve(provider,model,event_time,total_context),
//                               {Input: pure, cache..., output, reasoning}, false)
//   total_tokens  = UNCHANGED (old_input + output == pure + cache + output)
//
// Usage:
//   reproject-pure-input --db-path ~/.aikey/data/control.db [--dry-run]
//   reproject-pure-input --dsn "postgres://..." [--dry-run]
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

const (
	oldVersion = "0.1.0"
	newVersion = "0.2.0"
)

func main() {
	dbPath := flag.String("db-path", "", "SQLite control.db path (Personal/Trial)")
	dsn := flag.String("dsn", "", "PostgreSQL DSN (Production)")
	dryRun := flag.Bool("dry-run", false, "print planned changes without writing")
	flag.Parse()

	var (
		raw     *sql.DB
		dialect string
		err     error
	)
	switch {
	case *dbPath != "":
		raw, err = sql.Open("sqlite", *dbPath)
		dialect = shared.DialectSQLite
	case *dsn != "":
		raw, err = sql.Open("postgres", *dsn)
		dialect = shared.DialectPostgres
	default:
		log.Fatal("one of --db-path or --dsn is required")
	}
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer raw.Close()
	db := shared.NewDB(raw, dialect)

	resolver, err := pricing.Load()
	if err != nil {
		log.Fatalf("load pricing resolver: %v", err)
	}

	if err := run(context.Background(), db, resolver, *dryRun); err != nil {
		log.Fatalf("reproject failed: %v", err)
	}
}

func run(ctx context.Context, db *shared.DB, resolver *pricing.Resolver, dryRun bool) error {
	// event_time → epoch millis (dialect-aware) for the resolver's price-history lookup.
	tsExpr := "event_time"
	if !db.IsSQLite() {
		tsExpr = "(EXTRACT(EPOCH FROM event_time)*1000)::bigint"
	}
	// Rows needing work: old (0.1.0, total→pure conversion) OR any priced row still
	// missing its per-row unit_prices_snapshot (audit re-derivability backfill —
	// e.g. rows priced before the rc.8 snapshot feature, or reprojected earlier
	// without storing it). Idempotent: once converted + snapshotted, neither
	// predicate matches on a re-run.
	rows, err := db.QueryContext(ctx, `
		SELECT event_id, COALESCE(provider_code,''), COALESCE(model,''), `+tsExpr+`,
		       COALESCE(org_id,''), input_tokens, cached_input_tokens,
		       cache_creation_input_tokens, output_tokens, reasoning_tokens,
		       COALESCE(projector_version,'')
		  FROM usage_fact_dwd
		 WHERE projector_version = ?
		    OR (unit_prices_snapshot IS NULL AND billable_amount IS NOT NULL)`, oldVersion)
	if err != nil {
		return fmt.Errorf("select rows: %w", err)
	}
	type rowT struct {
		eventID                                    string
		provider, model                            string
		ts                                         int64
		org                                        string
		input, cached, creation, output, reasoning int64
		pver                                       string
	}
	var batch []rowT
	for rows.Next() {
		var r rowT
		if err := rows.Scan(&r.eventID, &r.provider, &r.model, &r.ts, &r.org,
			&r.input, &r.cached, &r.creation, &r.output, &r.reasoning, &r.pver); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var converted, snapBackfill, repriced, unpriced int
	for _, r := range batch {
		old := r.pver == oldVersion
		// old (0.1.0): input WAS the total → convert to pure, tier off the total.
		// new (already 0.2.0): input is already pure → don't subtract again; the tier
		// keys off the reconstructed total (pure + cache).
		var pure, totalContext int64
		if old {
			pure = r.input - r.cached - r.creation
			if pure < 0 {
				pure = 0
			}
			totalContext = r.input
		} else {
			pure = r.input
			totalContext = r.input + r.cached + r.creation
		}

		var billable sql.NullString
		var snap sql.NullString
		var cur sql.NullString // 'USD' when repriced — match the enricher (fact.Currency="USD")
		if up, lerr := resolver.Lookup(r.provider, r.model, r.ts, totalContext, r.org); lerr == nil {
			cost := pricing.ComputeCost(*up, pricing.TokenCounts{
				Input: pure, Output: r.output, CacheRead: r.cached,
				CacheCreation: r.creation, Reasoning: r.reasoning,
			}, false /* input is now pure */)
			billable = sql.NullString{String: pricing.FormatCostUSD(cost), Valid: true}
			cur = sql.NullString{String: "USD", Valid: true}
			// Snapshot the effective rates per row so cost is re-derivable from the
			// row alone (audit), matching the enricher's unit_prices_snapshot.
			if b, mErr := json.Marshal(up); mErr == nil {
				snap = sql.NullString{String: string(b), Valid: true}
			}
			repriced++
		} else {
			unpriced++ // leave billable NULL (unknown model); input still converted
		}
		if old {
			converted++
		} else {
			snapBackfill++
		}

		if dryRun {
			fmt.Printf("[dry-run] %s %s input %d→%d cache(%d+%d) billable=%s\n",
				r.eventID, r.model, r.input, pure, r.cached, r.creation,
				nullStr(billable))
			continue
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE usage_fact_dwd
			   SET input_tokens = ?, billable_amount = ?, unit_prices_snapshot = COALESCE(?, unit_prices_snapshot),
			       currency = COALESCE(?, currency), projector_version = ?
			 WHERE event_id = ?`,
			pure, billable, snap, cur, newVersion, r.eventID); err != nil {
			return fmt.Errorf("update %s: %w", r.eventID, err)
		}
	}
	mode := "applied"
	if dryRun {
		mode = "dry-run"
	}
	fmt.Printf("reproject %s: %d rows → converted(0.1.0→pure)=%d snapshot-backfill=%d repriced=%d unpriced(left NULL)=%d → %s\n",
		mode, len(batch), converted, snapBackfill, repriced, unpriced, newVersion)
	return nil
}

func nullStr(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return "NULL"
}
