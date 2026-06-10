package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// These cover the missing-identity / missing-org rejection on usage endpoints
// that previously had zero coverage. Every personal endpoint must 400 when no
// seat_id / account_id / org_id is supplied (parsePersonalParams gate) — without
// this guard a query with no identity could fall through to an unscoped repo
// read and leak another user's usage. Master endpoints 400 without org_id
// (parseMasterParams gate). Mirrors TestPersonalByModelTotal_MissingIdentity.

func assert400NoIdentity(t *testing.T, path string, h http.HandlerFunc) {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", path, nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("%s without identity: status=%d, want 400", path, w.Code)
	}
}

func TestPersonalByProtocolTimeline_MissingIdentity(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	assert400NoIdentity(t, "/v1/usage/personal/by-protocol/timeline", h.PersonalByProtocolTimeline)
}

func TestPersonalByProtocolHourly_MissingIdentity(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	assert400NoIdentity(t, "/v1/usage/personal/by-protocol/hourly", h.PersonalByProtocolHourly)
}

func TestPersonalByAppTotal_MissingIdentity(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	assert400NoIdentity(t, "/v1/usage/personal/by-app/total", h.PersonalByAppTotal)
}

func TestPersonalBySessionTotal_MissingIdentity(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	assert400NoIdentity(t, "/v1/usage/personal/by-session/total", h.PersonalBySessionTotal)
}

func TestPersonalRecent_MissingIdentity(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	assert400NoIdentity(t, "/v1/usage/personal/recent", h.PersonalRecent)
}

func TestPersonalUsageDetail_MissingIdentity(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	assert400NoIdentity(t, "/v1/usage/personal/detail", h.PersonalUsageDetail)
}

func TestPersonalByKeyTotal_MissingIdentity(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	assert400NoIdentity(t, "/v1/usage/personal/by-key/total", h.PersonalByKeyTotal)
}

func TestMasterByProtocolTotal_MissingOrgID(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	assert400NoIdentity(t, "/v1/usage/master/by-protocol/total", h.MasterByProtocolTotal)
}
