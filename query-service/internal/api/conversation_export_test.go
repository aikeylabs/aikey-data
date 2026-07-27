package api

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/conversation"
)

// fakeConvRepo serves canned data so the export render (Markdown + .zip) is
// tested without a DB. Only the export-path methods return data; the list
// methods are unused here.
type fakeConvRepo struct {
	system  map[string]string
	turns   map[string][]conversation.ThreadTurn
	sessIDs []string
}

func (f *fakeConvRepo) SeatSummaries(context.Context, conversation.QueryParams) ([]conversation.SeatSummary, int64, error) {
	return nil, 0, nil
}
func (f *fakeConvRepo) SessionSummaries(context.Context, conversation.QueryParams) ([]conversation.SessionSummary, int64, error) {
	return nil, 0, nil
}
func (f *fakeConvRepo) ThreadDetail(context.Context, conversation.QueryParams) (*conversation.ThreadDetail, error) {
	return nil, nil
}
func (f *fakeConvRepo) SessionSystemText(_ context.Context, p conversation.QueryParams) (string, error) {
	return f.system[p.SessionID], nil
}
func (f *fakeConvRepo) StreamSessionIDs(_ context.Context, _ conversation.QueryParams, fn func(string) error) error {
	for _, id := range f.sessIDs {
		if err := fn(id); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeConvRepo) StreamSessionTurns(_ context.Context, p conversation.QueryParams, fn func(*conversation.ThreadTurn) error) error {
	for i := range f.turns[p.SessionID] {
		tn := f.turns[p.SessionID][i]
		if err := fn(&tn); err != nil {
			return err
		}
	}
	return nil
}

func TestExport_SingleSessionMarkdown(t *testing.T) {
	repo := &fakeConvRepo{
		system: map[string]string{"s1": "be helpful"},
		turns: map[string][]conversation.ThreadTurn{"s1": {
			{EventID: "e1", CreatedAt: 1_700_000_000_000, Model: "claude-x", RequestStatus: "ok", UserText: "hello", AssistantText: "hi there"},
		}},
	}
	h := NewConversationHandler(repo)
	req := httptest.NewRequest("GET", "/v1/conversation-audit/export?org_id=o1&owner_account_id=a1&session_id=s1", nil)
	rec := httptest.NewRecorder()
	h.Export(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content-type=%q want text/markdown", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "conversation-s1.md") {
		t.Fatalf("content-disposition=%q want conversation-s1.md", cd)
	}
	body := rec.Body.String()
	for _, want := range []string{"# Conversation — session s1", "## System prompt", "be helpful", "## Turn 1", "claude-x", "**User**", "hello", "**Assistant**", "hi there"} {
		if !strings.Contains(body, want) {
			t.Fatalf("markdown missing %q in:\n%s", want, body)
		}
	}
}

func TestExport_SeatZip(t *testing.T) {
	repo := &fakeConvRepo{
		sessIDs: []string{"s1", "s2"},
		system:  map[string]string{"s1": "sys1"},
		turns: map[string][]conversation.ThreadTurn{
			"s1": {{EventID: "e1", UserText: "u1", AssistantText: "a1"}},
			"s2": {{EventID: "e2", UserText: "u2", AssistantText: "a2"}},
		},
	}
	h := NewConversationHandler(repo)
	// No session_id → seat .zip mode.
	req := httptest.NewRequest("GET", "/v1/conversation-audit/export?org_id=o1&owner_account_id=a1", nil)
	rec := httptest.NewRecorder()
	h.Export(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("content-type=%q want application/zip", ct)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("zip read: %v", err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("zip entries=%d want 2 (one .md per session)", len(zr.File))
	}
	names := map[string]bool{}
	for _, zf := range zr.File {
		names[zf.Name] = true
		rc, _ := zf.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !strings.Contains(string(b), "# Conversation — session ") {
			t.Fatalf("zip entry %s is not a conversation markdown:\n%s", zf.Name, b)
		}
	}
	if !names["s1.md"] || !names["s2.md"] {
		t.Fatalf("zip entry names=%v want s1.md + s2.md", names)
	}
}
