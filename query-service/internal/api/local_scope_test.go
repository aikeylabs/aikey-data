package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 🔴 LocalAllScope is a DEPLOYMENT capability, never a client claim
// (2026-08-20). The personal desktop app needs "all usage on this machine"
// across personal / team / OAuth identities; a shared team database must
// never answer that question, or one user reads another's usage.
func TestLocalAllScope_IsNotSettableFromTheQueryString(t *testing.T) {
	h := NewUsageHandler(nil) // ordinary (team/cluster) construction
	for _, qs := range []string{
		"account_id=x&scope=all",
		"account_id=x&local_all=1",
		"account_id=x&LocalAllScope=true",
		"org_id=personal&scope=all",
	} {
		r := httptest.NewRequest("GET", "/v1/usage/personal/hourly?"+qs, nil)
		p, err := h.personalParams(r)
		if err != nil {
			t.Fatalf("params %q: %v", qs, err)
		}
		if p.LocalAllScope {
			t.Fatalf("query string %q switched on the all-scope — a team deployment "+
				"would serve every user's usage to whoever asks", qs)
		}
	}
}

// Capability AND request, both required. The personal facade may answer the
// question; only a caller that ASKS gets the wider answer.
func TestLocalAllScope_NeedsCapabilityAndExplicitRequest(t *testing.T) {
	h := NewPersonalUsageHandler(nil)

	// 🔴 The Personal web's Overview charts call this same endpoint WITHOUT
	// scope=all and must keep their existing personal-key figures
	// (用户 2026-08-20: "不要影响 Personal web 端").
	r := httptest.NewRequest("GET", "/v1/usage/personal/hourly?org_id=personal", nil)
	p, err := h.personalParams(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.LocalAllScope {
		t.Fatal("the personal facade widened a plain request — every existing consumer, " +
			"including the Personal web, would silently change what it reports")
	}

	r = httptest.NewRequest("GET", "/v1/usage/personal/hourly?org_id=personal&scope=all", nil)
	p, err = h.personalParams(r)
	if err != nil {
		t.Fatal(err)
	}
	if !p.LocalAllScope {
		t.Fatal("an explicit scope=all on the personal facade was ignored — the app under-reports team/OAuth usage")
	}
}

// Every personal endpoint must route through personalParams: a handler that
// calls parsePersonalParams directly silently drops the scope and reports a
// fraction as the total.
func TestPersonalEndpoints_AllGoThroughThePersonalParamsWrapper(t *testing.T) {
	src := handlersSource(t)
	if n := strings.Count(src, "parsePersonalParams(r)"); n != 1 {
		t.Fatalf("parsePersonalParams(r) is called %d times; exactly ONE call "+
			"(inside personalParams) is allowed — every other endpoint must use "+
			"h.personalParams(r) or it loses LocalAllScope", n)
	}
}
