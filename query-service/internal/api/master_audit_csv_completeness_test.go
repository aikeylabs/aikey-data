package api

// master_audit_csv_completeness_test.go — the fence for the audit export.
//
// # Why this file exists
//
// `MasterUsageAuditRow`'s own docstring promises "the page shows a trimmed
// subset; the CSV export carries every field". That promise was false for four
// days: the SQL projection selected `fallback_attempt` / `fallback_reason`, the
// struct carried them, and `masterAuditCSVHeader` — a hand-maintained slice with
// nothing tying it to the struct — did not. An administrator exporting the audit
// for a billing dispute got a file with no record of which upstream actually
// served the request or why it switched away from the primary.
//
// 🔴 The failure mode is what makes it worth a fence rather than a fix. Adding a
// field to the struct compiles, ships, appears in the JSON API, and the export
// stays green — there is no step at which anything goes red. The only signal is
// a column an auditor does not know to look for.
//
// So the assertion is structural, not a list of names: EVERY exported field of
// `MasterUsageAuditRow` must appear in the header, matched by its `json` tag.
// A new field fails this test until someone decides where it goes in the export.
//
// # Why the header/record arity is checked too
//
// The two slices are written by hand, in separate functions, forty lines apart.
// A header entry with no matching record entry (or the reverse) shifts every
// column after it — and encoding/csv does not care, it writes whatever length it
// is given. The result is a file where `content_hash` holds a source id, which
// reads as data corruption rather than a code defect.

import (
	"reflect"
	"testing"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

// csvColumnFor maps a struct field whose export column is deliberately named
// something else. Two entries, both for the same reason: the JSON API hands the
// browser epoch millis (`*_ms`) because JavaScript formats them in the viewer's
// timezone, while the CSV renders RFC3339 UTC because a spreadsheet has no
// viewer and a bare integer is unreadable in one.
//
// 🔴 An explicit map, not a prefix rule and not a skip. A rename is a decision
// somebody made; a field that merely went missing is a defect. Collapsing the
// two into "close enough" is how the fence would stop catching the thing it was
// written for. Anything not listed here must appear under its own json tag.
var csvColumnFor = map[string]string{
	"event_time_ms":  "event_time",
	"occurred_at_ms": "occurred_at",
}

func TestMasterAuditCSV_CarriesEveryFieldOfTheRow(t *testing.T) {
	inHeader := make(map[string]struct{}, len(masterAuditCSVHeader))
	for _, c := range masterAuditCSVHeader {
		inHeader[c] = struct{}{}
	}

	rt := reflect.TypeOf(usage.MasterUsageAuditRow{})
	var missing []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// Strip any ",omitempty" and friends.
		for j := 0; j < len(tag); j++ {
			if tag[j] == ',' {
				tag = tag[:j]
				break
			}
		}
		col := tag
		if alias, renamed := csvColumnFor[tag]; renamed {
			col = alias
		}
		if _, ok := inHeader[col]; !ok {
			missing = append(missing, tag)
		}
	}

	// The alias map must not outlive its entries: a stale alias silently excuses
	// a field that later goes missing under its real name.
	for tag, col := range csvColumnFor {
		if _, ok := inHeader[col]; !ok {
			t.Errorf("csvColumnFor says %q is exported as %q, but no such column exists — "+
				"drop the alias or fix the header", tag, col)
		}
	}

	if len(missing) > 0 {
		t.Errorf("masterAuditCSVHeader is missing %d field(s) of MasterUsageAuditRow: %v\n\n"+
			"The struct's docstring promises the CSV carries EVERY field, and the export is\n"+
			"what an administrator keeps when they need to reconstruct a charge months later.\n"+
			"A field that reaches the JSON API but not the export is invisible in exactly the\n"+
			"artifact that outlives the session.\n\n"+
			"Add each name to masterAuditCSVHeader AND a matching value to\n"+
			"masterAuditCSVRecord, in the same position, at the END of both slices —\n"+
			"importers address these columns positionally.", len(missing), missing)
	}
}

func TestMasterAuditCSV_HeaderAndRecordAgreeOnWidth(t *testing.T) {
	// A row with every pointer field populated, so the record builder takes its
	// non-nil branches — a nil-everything row would exercise only the "" paths
	// and could hide an arity bug behind a shorter slice.
	seq := int64(7)
	attempt := int64(2)
	amount := "1.2345"
	row := &usage.MasterUsageAuditRow{
		SourceSeq:       &seq,
		FallbackAttempt: &attempt,
		BillableAmount:  &amount,
		FallbackReason:  "UPSTREAM_SERVER_ERROR",
	}

	got := len(masterAuditCSVRecord(row))
	if want := len(masterAuditCSVHeader); got != want {
		t.Fatalf("masterAuditCSVRecord emits %d values but masterAuditCSVHeader declares %d columns.\n\n"+
			"encoding/csv writes whatever length it is handed, so a mismatch does not fail —\n"+
			"it shifts every column after the divergence. The export then looks like corrupted\n"+
			"data rather than a code defect, and the person reading it has no reason to suspect\n"+
			"the writer.", got, want)
	}
}

// The NULL/0 distinction the pointer exists for must survive into the export.
//
// Folding a nil FallbackAttempt to "0" (or "1") would make "this key has no
// upstream chain at all" indistinguishable from "the primary served it" — the
// precise distinction the drill-down exists to make, and one the aggregate
// step-around view already relies on (`fallback_attempt IS NOT NULL AND > 1`).
// A zero in a spreadsheet reads as a measurement, not as an absence.
func TestMasterAuditCSV_NullFallbackAttemptIsBlankNotZero(t *testing.T) {
	idx := -1
	for i, c := range masterAuditCSVHeader {
		if c == "fallback_attempt" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("fallback_attempt is not in masterAuditCSVHeader")
	}

	noChain := masterAuditCSVRecord(&usage.MasterUsageAuditRow{})
	if noChain[idx] != "" {
		t.Errorf("a row with no chain exported fallback_attempt=%q, want \"\" — "+
			"a number here claims a measurement that was never made", noChain[idx])
	}

	served := int64(1)
	primary := masterAuditCSVRecord(&usage.MasterUsageAuditRow{FallbackAttempt: &served})
	if primary[idx] != "1" {
		t.Errorf("a primary-served row exported fallback_attempt=%q, want \"1\"", primary[idx])
	}
}
