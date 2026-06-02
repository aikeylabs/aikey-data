package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

// adminMockRepo implements the narrow adminRepo interface with injectable
// behaviour so handler tests can drive the 200 / 400 / 404 branches.
type adminMockRepo struct {
	listFn   func(ctx context.Context, status string) ([]usage.UnpricedModel, error)
	updateFn func(ctx context.Context, provider, model, status string) error
	auditFn  func(ctx context.Context, eventID string) (*usage.EventAudit, error)
}

func (m *adminMockRepo) ListUnpricedModels(ctx context.Context, status string) ([]usage.UnpricedModel, error) {
	return m.listFn(ctx, status)
}
func (m *adminMockRepo) UpdateUnpricedModelStatus(ctx context.Context, provider, model, status string) error {
	return m.updateFn(ctx, provider, model, status)
}
func (m *adminMockRepo) GetEventAudit(ctx context.Context, eventID string) (*usage.EventAudit, error) {
	return m.auditFn(ctx, eventID)
}

func TestAdminListUnpricedModels(t *testing.T) {
	h := &AdminHandler{repo: &adminMockRepo{
		listFn: func(_ context.Context, status string) ([]usage.UnpricedModel, error) {
			return []usage.UnpricedModel{{Model: "gpt-4o-2024-08-06", Provider: "openai", EventCount: 3, Status: "pending"}}, nil
		},
	}}

	// Happy path.
	req := httptest.NewRequest("GET", "/v1/admin/unpriced-models?status=pending", nil)
	w := httptest.NewRecorder()
	h.ListUnpricedModels(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var body struct {
		Models []usage.UnpricedModel `json:"models"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Models) != 1 || body.Models[0].Model != "gpt-4o-2024-08-06" {
		t.Errorf("unexpected models payload: %+v", body.Models)
	}

	// Bad status filter → 400.
	req = httptest.NewRequest("GET", "/v1/admin/unpriced-models?status=bogus", nil)
	w = httptest.NewRecorder()
	h.ListUnpricedModels(w, req)
	if w.Code != 400 {
		t.Errorf("bad status filter want 400, got %d", w.Code)
	}
}

func TestAdminUpdateUnpricedModelStatus(t *testing.T) {
	// Valid update → 200.
	h := &AdminHandler{repo: &adminMockRepo{
		updateFn: func(_ context.Context, provider, model, status string) error { return nil },
	}}
	req := httptest.NewRequest("POST", "/v1/admin/unpriced-models/openai/gpt-4o?status=acknowledged", nil)
	req.SetPathValue("provider", "openai")
	req.SetPathValue("model", "gpt-4o")
	w := httptest.NewRecorder()
	h.UpdateUnpricedModelStatus(w, req)
	if w.Code != 200 {
		t.Fatalf("valid update want 200, got %d", w.Code)
	}

	// Invalid status value → 400 (repo not even called).
	req = httptest.NewRequest("POST", "/v1/admin/unpriced-models/openai/gpt-4o?status=bogus", nil)
	req.SetPathValue("provider", "openai")
	req.SetPathValue("model", "gpt-4o")
	w = httptest.NewRecorder()
	h.UpdateUnpricedModelStatus(w, req)
	if w.Code != 400 {
		t.Errorf("invalid status want 400, got %d", w.Code)
	}

	// Unknown row → repo returns ErrNotFound → 404.
	h404 := &AdminHandler{repo: &adminMockRepo{
		updateFn: func(_ context.Context, provider, model, status string) error { return usage.ErrNotFound },
	}}
	req = httptest.NewRequest("POST", "/v1/admin/unpriced-models/openai/nope?status=fixed", nil)
	req.SetPathValue("provider", "openai")
	req.SetPathValue("model", "nope")
	w = httptest.NewRecorder()
	h404.UpdateUnpricedModelStatus(w, req)
	if w.Code != 404 {
		t.Errorf("unknown row want 404, got %d", w.Code)
	}
}

func TestAdminGetEventAudit(t *testing.T) {
	// Happy path.
	h := &AdminHandler{repo: &adminMockRepo{
		auditFn: func(_ context.Context, eventID string) (*usage.EventAudit, error) {
			return &usage.EventAudit{EventID: eventID, Model: "gpt-4o-2024-08-06", ProviderCode: "openai"}, nil
		},
	}}
	req := httptest.NewRequest("GET", "/v1/admin/events/e1/audit", nil)
	req.SetPathValue("event_id", "e1")
	w := httptest.NewRecorder()
	h.GetEventAudit(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var a usage.EventAudit
	if err := json.NewDecoder(w.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	if a.EventID != "e1" {
		t.Errorf("event_id = %q, want e1", a.EventID)
	}

	// Unknown event → 404.
	h404 := &AdminHandler{repo: &adminMockRepo{
		auditFn: func(_ context.Context, eventID string) (*usage.EventAudit, error) {
			return nil, usage.ErrNotFound
		},
	}}
	req = httptest.NewRequest("GET", "/v1/admin/events/nope/audit", nil)
	req.SetPathValue("event_id", "nope")
	w = httptest.NewRecorder()
	h404.GetEventAudit(w, req)
	if w.Code != 404 {
		t.Errorf("unknown event want 404, got %d", w.Code)
	}
}
