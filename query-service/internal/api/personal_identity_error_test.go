package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPersonalIdentityErrorNamesEveryAcceptedParam keeps the 400 body honest
// about which identities a personal-scope query accepts.
//
// WHY this exists: the message used to read "seat_id or account_id is
// required" while the line right above it also accepted org_id=personal --
// which is what the Personal web client actually sends, because personal
// events carry no account_id. The omission is not cosmetic. A caller who
// believes the text and supplies a made-up account_id does not get an error;
// they get HTTP 200 and an empty array, which reads as an authoritative "you
// used nothing today". Wrong answers that look authoritative are the failure
// mode this project treats as worse than a crash, and the error text is the
// only place the distinction is visible.
//
// The fence is written against the PARSER, not against a hardcoded string:
// every name the message advertises is fed to parsePersonalParams on its own
// and must be accepted. So the message cannot advertise an identity the code
// stopped honouring, and renaming an accepted param without updating the text
// fails here rather than in a user's support ticket.
//
// 🔴 Known limit, stated rather than implied: this catches a message that
// LIES, not a message that is INCOMPLETE. Adding a fourth accepted identity
// without naming it in personalIdentityParamsMsg still passes -- the parser
// has no enumerable list to diff against. Fixing that properly means making
// the acceptance check iterate the same list the message is built from, which
// is a bigger change than this one and has not been made.
func TestPersonalIdentityErrorNamesEveryAcceptedParam(t *testing.T) {
	// No identity at all: the caller must be told every way out, not one of them.
	req := httptest.NewRequest("GET", "/v1/usage/personal/by-protocol/total", nil)
	_, err := parsePersonalParams(req)
	if err == nil {
		t.Fatal("a request with no identity was accepted; personal scope is a filter, not a default")
	}
	msg := err.Error()
	for _, name := range []string{"seat_id", "account_id", "org_id=personal"} {
		if !strings.Contains(msg, name) {
			t.Errorf("the missing-identity error does not mention %q.\n"+
				"got: %s\n"+
				"Every accepted identity must appear here: a caller who cannot see "+
				"their option will invent one, and an invented identity returns 200 "+
				"with an empty array instead of an error.", name, msg)
		}
	}

	// Each advertised identity must actually work on its own.
	for _, tc := range []struct{ name, query string }{
		{"seat_id", "seat_id=seat-1"},
		{"account_id", "account_id=acct-1"},
		{"org_id=personal", "org_id=personal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/usage/personal/by-protocol/total?"+tc.query, nil)
			if _, err := parsePersonalParams(req); err != nil {
				t.Fatalf("%s is advertised in the error message but the parser rejects it: %v.\n"+
					"The message and the code have drifted apart; whichever is wrong, "+
					"a caller following the message would be misled.", tc.name, err)
			}
		})
	}
}
