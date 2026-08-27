package api

import (
	"net/http/httptest"
	"testing"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

// The wire spelling of "give me every row" is `?limit=all`.
//
// 🔴 Spelled as a word rather than as 0 or -1 on purpose: in a URL and in an
// access log, `limit=0` is indistinguishable from a caller that forgot, and a
// reviewer cannot tell a deliberate full scan from a bug. The cost ledger's
// per-department table is the caller that needs it — a top-N slice cannot add
// up to the organisation total for any N.
//
// bugfix: workflow/CI/bugfix/2026-08-27-ledger-by-department-top-n-truncation.md
func TestParseMasterParams_LimitAll(t *testing.T) {
	cases := []struct {
		query string
		want  int
		why   string
	}{
		{"?org_id=o1&limit=all", usage.LimitAll, "the documented spelling"},
		{"?org_id=o1&limit=ALL", usage.LimitAll, "case must not decide behaviour"},
		{"?org_id=o1&limit=20", 20, "numeric limits keep working unchanged"},
		{"?org_id=o1", 50, "absent stays the established default"},
		{"?org_id=o1&limit=0", 50, "0 is not a synonym for all — it reads as 'forgot'"},
		{"?org_id=o1&limit=-3", 50, "a negative number is not a back door to LimitAll"},
		{"?org_id=o1&limit=banana", 50, "unparseable falls back, it does not unlock everything"},
	}
	for _, c := range cases {
		p, err := parseMasterParams(httptest.NewRequest("GET", "/v1/usage/master/ranking"+c.query, nil))
		if err != nil {
			t.Fatalf("%s: %v", c.query, err)
		}
		if p.Limit != c.want {
			t.Errorf("%s: Limit=%d, want %d (%s)", c.query, p.Limit, c.want, c.why)
		}
	}
}
