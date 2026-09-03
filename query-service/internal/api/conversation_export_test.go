package api

import (
	"encoding/json"
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

// TestFence_13F5_TheExportCarriesTheToolCalls.
//
// 🔴 A page that shows tool calls and an export that does not makes the export
// an INCOMPLETE piece of audit evidence — and the export is the artefact that
// leaves the building, gets attached to a ticket, and is read months later by
// somebody who cannot open the console (task 13.7d).
//
// It also asserts the argument SUMMARY reaches the file and the VALUE does not.
// The export travels furthest, so a leak there is the most expensive one.
func TestFence_13F5_TheExportCarriesTheToolCalls(t *testing.T) {
	repo := &fakeConvRepo{
		turns: map[string][]conversation.ThreadTurn{"s1": {{
			EventID: "e1", CreatedAt: 1_700_000_000_000, RequestStatus: "ok",
			UserText: "check the db", AssistantText: "looking",
			ToolCalls: json.RawMessage(`[{"tool_call_id":"t1","tool_name":"query_readonly",` +
				`"args_digest":[{"key":"sql","type":"string","len":41}],"link_state":"bypassed"}]`),
		}}},
	}
	h := NewConversationHandler(repo)
	req := httptest.NewRequest("GET", "/v1/conversation-audit/export?org_id=o1&owner_account_id=a1&session_id=s1", nil)
	rec := httptest.NewRecorder()
	h.Export(rec, req)
	body := rec.Body.String()

	for _, want := range []string{"**Tool calls**", "query_readonly", "sql", "string", "41"} {
		if !strings.Contains(body, want) {
			t.Fatalf("🔴 the export dropped %q. A page that shows tool calls and an export "+
				"that does not is an incomplete audit record:\n%s", want, body)
		}
	}
	// 🔴 `bypassed` must survive into the file, and it must not read like a
	// routine state: it says the call never went through AiKey.
	if !strings.Contains(body, "NOT ROUTED THROUGH AIKEY") {
		t.Errorf("🔴 a bypassed call was exported without saying so. `pending` and `bypassed` "+
			"are opposite facts and only the second is a security finding:\n%s", body)
	}
}

// TestFence_13F5_AnUncollectedTurnSaysSoRatherThanClaimingNone.
//
// 🔴 Task 13.8 in the export. A turn captured by an older proxy carries NO
// tool_calls field, and writing "None." for it would put a claim nobody
// established into a file that gets treated as evidence.
func TestFence_13F5_AnUncollectedTurnSaysSoRatherThanClaimingNone(t *testing.T) {
	repo := &fakeConvRepo{
		turns: map[string][]conversation.ThreadTurn{"s1": {{
			EventID: "e1", CreatedAt: 1_700_000_000_000, RequestStatus: "ok",
			UserText: "hi", AssistantText: "hello",
			// ToolCalls deliberately absent — an older proxy.
		}}},
	}
	h := NewConversationHandler(repo)
	req := httptest.NewRequest("GET", "/v1/conversation-audit/export?org_id=o1&owner_account_id=a1&session_id=s1", nil)
	rec := httptest.NewRecorder()
	h.Export(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "Not collected") {
		t.Fatalf("🔴 a turn from a non-collecting proxy was exported without saying so:\n%s", body)
	}
	if strings.Contains(body, "_None._") {
		t.Fatalf("🔴 the export claimed 'None' for a turn nobody examined. That is a false "+
			"report, not a missing feature:\n%s", body)
	}
}

// TestAnEmptyToolCallListIsReportedAsNone is the other side: a COLLECTING proxy
// that saw no calls must say "none", not "not collected" — otherwise every
// ordinary turn would carry a warning nobody needs to act on.
func TestAnEmptyToolCallListIsReportedAsNone(t *testing.T) {
	repo := &fakeConvRepo{
		turns: map[string][]conversation.ThreadTurn{"s1": {{
			EventID: "e1", CreatedAt: 1_700_000_000_000, RequestStatus: "ok",
			UserText: "hi", AssistantText: "hello", ToolCalls: json.RawMessage(`[]`),
		}}},
	}
	h := NewConversationHandler(repo)
	req := httptest.NewRequest("GET", "/v1/conversation-audit/export?org_id=o1&owner_account_id=a1&session_id=s1", nil)
	rec := httptest.NewRecorder()
	h.Export(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "_None._") {
		t.Fatalf("a collecting proxy that saw no calls should report none:\n%s", body)
	}
	if strings.Contains(body, "Not collected") {
		t.Fatalf("🔴 an empty list was reported as 'not collected'. Every ordinary turn would "+
			"then carry a warning, and the warning would stop meaning anything:\n%s", body)
	}
}
