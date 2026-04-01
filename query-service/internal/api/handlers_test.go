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

func (m *mockRepo) PersonalByProtocolTimeline(_ context.Context, p usage.QueryParams) ([]usage.ProtocolTimelinePoint, error) {
	return []usage.ProtocolTimelinePoint{
		{Date: "2026-03-01", ProtocolType: "openai", TotalTokens: 800, RequestCount: 4},
		{Date: "2026-03-01", ProtocolType: "anthropic", TotalTokens: 200, RequestCount: 1},
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
