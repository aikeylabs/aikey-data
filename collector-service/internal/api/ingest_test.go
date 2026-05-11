package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

// capturingODS records every event the handler hands down so tests can
// assert what account_id actually landed (post-middleware override).
type capturingODS struct {
	mu      sync.Mutex
	events  []ingest.UsageEvent
}

func (c *capturingODS) InsertEvent(_ context.Context, e *ingest.UsageEvent, _ []byte) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *e
	c.events = append(c.events, cp)
	return true, nil
}

func (c *capturingODS) snapshot() []ingest.UsageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ingest.UsageEvent, len(c.events))
	copy(out, c.events)
	return out
}

// newHandlerWithSubject constructs an IngestHandler wrapped in a tiny
// http.Handler that injects the given IngestSubject onto every request
// context before delegating. Lets tests exercise the post-middleware
// state without standing up the full IngestAuth pipeline.
func newHandlerWithSubject(svc *ingest.Service, subj *shared.IngestSubject) http.Handler {
	h := NewIngestHandler(svc)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if subj != nil {
			ctx = shared.WithIngestSubject(ctx, *subj)
		}
		h.HandleBatch(w, r.WithContext(ctx))
	})
}

func mkEvent(id, accountID string) ingest.UsageEvent {
	now := aikeytime.FromTime(time.Now())
	return ingest.UsageEvent{
		EventID:       id,
		OrgID:         "org1",
		AccountID:     accountID,
		EventTime:     now,
		OccurredAt:    now,
		RequestStatus: "success",
		RequestCount:  1,
	}
}

func postBatch(t *testing.T, h http.Handler, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/usage-events:batch", bytes.NewReader(buf))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// ── account_id force-override contract ────────────────────────────────

// 2026-05-11 B-phase contract: when ingest is authenticated via user JWT,
// every event's AccountID is overwritten with the JWT-attested value —
// regardless of what the client claimed.
func TestHandleBatch_JWTSubject_ForceOverwriteClientClaimedAccountID(t *testing.T) {
	ods := &capturingODS{}
	svc := ingest.NewService(ods)
	h := newHandlerWithSubject(svc, &shared.IngestSubject{
		Source: "jwt", AccountID: "acct-jwt",
	})

	resp := postBatch(t, h, ingest.BatchRequest{
		Source: "aikey-proxy",
		Events: []ingest.UsageEvent{
			// Client claims account-X — must be overwritten.
			mkEvent("e1", "acct-X-forged"),
			// Client claims acct-jwt — same as JWT, no semantic change.
			mkEvent("e2", "acct-jwt"),
			// Client leaves AccountID empty — JWT value still applied.
			mkEvent("e3", ""),
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got := ods.snapshot()
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}
	for _, ev := range got {
		if ev.AccountID != "acct-jwt" {
			t.Errorf("event %s: AccountID=%q, want acct-jwt (force-overwrite)",
				ev.EventID, ev.AccountID)
		}
	}
}

// service_token escape hatch: client-claimed AccountID is honoured
// verbatim. This is the admin trust mode — the deployer chose to grant
// the bearer permission to forge any account_id.
func TestHandleBatch_ServiceTokenSubject_TrustsClientClaimedAccountID(t *testing.T) {
	ods := &capturingODS{}
	svc := ingest.NewService(ods)
	h := newHandlerWithSubject(svc, &shared.IngestSubject{
		Source: "service_token", // no AccountID — middleware doesn't attach one
	})

	resp := postBatch(t, h, ingest.BatchRequest{
		Source: "aikey-proxy",
		Events: []ingest.UsageEvent{
			mkEvent("e1", "claim-A"),
			mkEvent("e2", "claim-B"),
		},
	})
	defer resp.Body.Close()

	got := ods.snapshot()
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	// Crucial: AccountID NOT overridden — preserves client claim
	// (this is the explicit semantics of the escape hatch).
	if got[0].AccountID != "claim-A" || got[1].AccountID != "claim-B" {
		t.Errorf("service_token path overrode AccountID; got %+v / %+v", got[0], got[1])
	}
}

// No subject (legacy / dev open-ingest path): same trust-mode semantics
// as service_token. Mirrors pre-B handlers that never knew about
// IngestSubject.
func TestHandleBatch_NoSubject_TrustsClientClaimedAccountID(t *testing.T) {
	ods := &capturingODS{}
	svc := ingest.NewService(ods)
	h := newHandlerWithSubject(svc, nil)

	resp := postBatch(t, h, ingest.BatchRequest{
		Source: "aikey-proxy",
		Events: []ingest.UsageEvent{mkEvent("e1", "claim-A")},
	})
	defer resp.Body.Close()

	got := ods.snapshot()
	if len(got) != 1 || got[0].AccountID != "claim-A" {
		t.Errorf("no-subject path should preserve client claim; got %+v", got)
	}
}

// Empty batch must error before any account_id processing — order-of-
// operations check, prevents a future regression where a no-events
// request might still trigger middleware-shaped side effects.
func TestHandleBatch_EmptyBatchRejectedBeforeOverride(t *testing.T) {
	ods := &capturingODS{}
	svc := ingest.NewService(ods)
	h := newHandlerWithSubject(svc, &shared.IngestSubject{
		Source: "jwt", AccountID: "acct-jwt",
	})

	resp := postBatch(t, h, ingest.BatchRequest{Events: nil})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("empty batch must 400; got %d", resp.StatusCode)
	}
	if len(ods.snapshot()) != 0 {
		t.Errorf("empty batch should not produce inserts")
	}
}
