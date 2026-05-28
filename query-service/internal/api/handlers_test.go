package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

// mockRepo implements usage.Repository for testing.
type mockRepo struct{}

func (m *mockRepo) PersonalTimeline(_ context.Context, p usage.QueryParams) ([]usage.TimelinePoint, error) {
	return []usage.TimelinePoint{
		{Date: "2026-03-01", TotalTokens: 1000, RequestCount: 5},
		{Date: "2026-03-02", TotalTokens: 2000, RequestCount: 10},
	}, nil
}

func (m *mockRepo) PersonalHourlyTimeline(_ context.Context, p usage.QueryParams) ([]usage.HourlyPoint, error) {
	return []usage.HourlyPoint{
		{Hour: 9, TotalTokens: 400, RequestCount: 2},
		{Hour: 14, TotalTokens: 1600, RequestCount: 8},
	}, nil
}

func (m *mockRepo) PersonalByProtocolTimeline(_ context.Context, p usage.QueryParams) ([]usage.ProtocolTimelinePoint, error) {
	return []usage.ProtocolTimelinePoint{
		{Date: "2026-03-01", ProtocolType: "openai", TotalTokens: 800, RequestCount: 4},
		{Date: "2026-03-01", ProtocolType: "anthropic", TotalTokens: 200, RequestCount: 1},
	}, nil
}

// PersonalByProtocolHourly — added 2026-05-28 alongside the new
// /v1/usage/personal/by-protocol/hourly endpoint that backs the "1D"
// range option on /user/usage-ledger. Returns a small fixture so any
// future handler test gets a non-empty payload.
func (m *mockRepo) PersonalByProtocolHourly(_ context.Context, p usage.QueryParams) ([]usage.ProtocolHourlyPoint, error) {
	return []usage.ProtocolHourlyPoint{
		{Hour: 9, ProtocolType: "anthropic", TotalTokens: 1200, RequestCount: 6},
		{Hour: 14, ProtocolType: "openai", TotalTokens: 800, RequestCount: 4},
	}, nil
}

func (m *mockRepo) PersonalByProtocolTotal(_ context.Context, p usage.QueryParams) ([]usage.ProtocolTotal, error) {
	return []usage.ProtocolTotal{
		{ProtocolType: "openai", TotalTokens: 5000, RequestCount: 25},
		{ProtocolType: "anthropic", TotalTokens: 3000, RequestCount: 15},
	}, nil
}

func (m *mockRepo) MasterUserRanking(_ context.Context, p usage.QueryParams) ([]usage.UserRanking, error) {
	return []usage.UserRanking{
		{AccountID: "acc1", SeatID: "seat1", TotalTokens: 10000, RequestCount: 50},
		{AccountID: "acc2", SeatID: "seat2", TotalTokens: 8000, RequestCount: 40},
	}, nil
}

func (m *mockRepo) MasterByProtocolTotal(_ context.Context, p usage.QueryParams) ([]usage.ProtocolTotal, error) {
	return []usage.ProtocolTotal{
		{ProtocolType: "openai", TotalTokens: 15000, RequestCount: 75},
	}, nil
}

func (m *mockRepo) MasterTimeline(_ context.Context, p usage.QueryParams) ([]usage.TimelinePoint, error) {
	return []usage.TimelinePoint{
		{Date: "2026-03-01", TotalTokens: 5000, RequestCount: 25},
	}, nil
}

func (m *mockRepo) PersonalByKeyTotal(_ context.Context, p usage.QueryParams) ([]usage.KeyTotal, error) {
	return []usage.KeyTotal{
		{VirtualKeyID: "personal:my-key", TotalTokens: 3000, RequestCount: 15},
		{VirtualKeyID: "personal:other-key", TotalTokens: 1500, RequestCount: 8},
	}, nil
}

func (m *mockRepo) PersonalByModelTotal(_ context.Context, p usage.QueryParams) ([]usage.ModelTotal, error) {
	return []usage.ModelTotal{
		{Model: "claude-sonnet-4-6", TotalTokens: 4000, RequestCount: 20, InputTokens: 2500, OutputTokens: 1500},
		{Model: "kimi-k2-0905-preview", TotalTokens: 1200, RequestCount: 6, InputTokens: 900, OutputTokens: 300},
	}, nil
}

// PersonalByAppTotal — added 2026-05-26 to restore Repository interface
// satisfaction after commit 18ad084 (`feat(query): add /v1/usage/personal/
// by-app/total endpoint`) added the method to the interface but did not
// update this mock. The interface signature is in
// query-service/internal/usage/repository.go. Two fixture rows mirror the
// pattern used by PersonalByModelTotal above so any future handler test
// that exercises PersonalByAppTotal gets a non-empty payload.
func (m *mockRepo) PersonalByAppTotal(_ context.Context, p usage.QueryParams) ([]usage.AppTotal, error) {
	return []usage.AppTotal{
		{AppSlug: "claude-code", ProviderCode: "anthropic", TotalTokens: 3500, RequestCount: 18},
		{AppSlug: "", ProviderCode: "openai", TotalTokens: 800, RequestCount: 4},
	}, nil
}

// PersonalBySessionTotal — added 2026-05-26 for the Performance Top N
// sessions chart endpoint. Returns a small fixture (one identified
// session + one "no session" bucket) so future handler tests get a
// non-empty payload. Interface signature lives in
// query-service/internal/usage/repository.go.
func (m *mockRepo) PersonalBySessionTotal(_ context.Context, p usage.QueryParams) ([]usage.SessionTotal, error) {
	return []usage.SessionTotal{
		{
			SessionID:          "session-abc-123",
			SampleVirtualKeyID: "oauth:session_xyz",
			SampleIdentity:     "user@example.com",
			SampleAppSlug:      "claude-code",
			TotalTokens:        12345,
			InputTokens:        9000,
			OutputTokens:       3345,
			RequestCount:       17,
		},
		{
			SessionID:    "", // "no session" bucket
			TotalTokens:  500,
			InputTokens:  400,
			OutputTokens: 100,
			RequestCount: 3,
		},
	}, nil
}

// PersonalRecent — Phase 3B R23 (2026-05-11). Returns a small fixture
// row so the handler test (if added later) sees a non-empty payload.
// Existing tests don't exercise the Recent path; this just satisfies
// the Repository interface contract.
func (m *mockRepo) PersonalRecent(_ context.Context, p usage.QueryParams) ([]usage.RecentRequest, error) {
	return []usage.RecentRequest{
		{
			RequestID:      "req-test",
			EventTimeMs:    1700000000000,
			ProviderCode:   "anthropic",
			Model:          "claude-test",
			TotalTokens:    100,
			HTTPStatusCode: 200,
			VirtualKeyID:   "personal:my-key",
			RequestStatus:  "success",
		},
	}, nil
}

func TestPersonalTimeline(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/personal/timeline?seat_id=seat1", nil)
	w := httptest.NewRecorder()
	h.PersonalTimeline(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result []usage.TimelinePoint
	json.NewDecoder(w.Body).Decode(&result)
	if len(result) != 2 {
		t.Errorf("expected 2 points, got %d", len(result))
	}
}

func TestPersonalTimeline_MissingSeatID(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/personal/timeline", nil)
	w := httptest.NewRecorder()
	h.PersonalTimeline(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestPersonalByProtocolTotal(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/personal/by-protocol/total?seat_id=seat1&start_date=2026-03-01&end_date=2026-03-31", nil)
	w := httptest.NewRecorder()
	h.PersonalByProtocolTotal(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result []usage.ProtocolTotal
	json.NewDecoder(w.Body).Decode(&result)
	if len(result) != 2 {
		t.Errorf("expected 2 protocols, got %d", len(result))
	}
}

func TestPersonalByModelTotal(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/personal/by-model/total?seat_id=seat1&start_date=2026-03-01&end_date=2026-03-31", nil)
	w := httptest.NewRecorder()
	h.PersonalByModelTotal(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result []usage.ModelTotal
	json.NewDecoder(w.Body).Decode(&result)
	if len(result) != 2 {
		t.Errorf("expected 2 models, got %d", len(result))
	}
}

func TestPersonalByModelTotal_MissingIdentity(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/personal/by-model/total", nil)
	w := httptest.NewRecorder()
	h.PersonalByModelTotal(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestMasterUserRanking(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/master/ranking?org_id=org1&limit=10", nil)
	w := httptest.NewRecorder()
	h.MasterUserRanking(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result []usage.UserRanking
	json.NewDecoder(w.Body).Decode(&result)
	if len(result) != 2 {
		t.Errorf("expected 2 users, got %d", len(result))
	}
}

func TestMasterTimeline(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/master/timeline?org_id=org1", nil)
	w := httptest.NewRecorder()
	h.MasterTimeline(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestMasterRanking_MissingOrgID(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/master/ranking", nil)
	w := httptest.NewRecorder()
	h.MasterUserRanking(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- PersonalHourlyTimeline ---

func TestPersonalHourlyTimeline_OK(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/personal/hourly?seat_id=seat1&date=2026-04-24", nil)
	w := httptest.NewRecorder()
	h.PersonalHourlyTimeline(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var result []usage.HourlyPoint
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("expected 2 points from mock, got %d", len(result))
	}
}

func TestPersonalHourlyTimeline_MissingIdentity(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/personal/hourly", nil)
	w := httptest.NewRecorder()
	h.PersonalHourlyTimeline(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 (no seat_id/account_id/org_id=personal), got %d", w.Code)
	}
}

func TestPersonalHourlyTimeline_PersonalEdition(t *testing.T) {
	h := NewUsageHandler(&mockRepo{})
	req := httptest.NewRequest("GET", "/v1/usage/personal/hourly?org_id=personal", nil)
	w := httptest.NewRecorder()
	h.PersonalHourlyTimeline(w, req)
	if w.Code != 200 {
		t.Fatalf("personal-edition path should accept org_id=personal: got %d body=%s", w.Code, w.Body.String())
	}
}
