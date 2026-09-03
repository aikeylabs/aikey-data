package api

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/conversation"
	"github.com/AiKeyLabs/pkg/mcpwire"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

// Export handles GET /v1/conversation-audit/export
//
//   - ?org_id=&owner_account_id=&session_id=                 → one session as Markdown
//   - ?org_id=&owner_account_id=&start_date=&end_date=       → the seat's sessions as a
//     .zip (one .md per session), date range optional
//
// Streaming (decision 10): the .md renders turn-by-turn off a DB cursor and the
// .zip writes one entry per session as it goes — neither buffers the whole export.
// Once the 200 + headers are sent a mid-stream error can't change the status, so
// such errors are logged (per session for the zip) and the stream ends.
func (h *ConversationHandler) Export(w http.ResponseWriter, r *http.Request) {
	p, err := parseConvParams(r, true, false) // owner required; session optional (selects mode)
	if err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", err.Error())
		return
	}
	if p.SessionID != "" {
		h.exportSession(w, r, p)
		return
	}
	h.exportSeatZip(w, r, p)
}

func (h *ConversationHandler) exportSession(w http.ResponseWriter, r *http.Request, p conversation.QueryParams) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="conversation-%s.md"`, safeFilename(p.SessionID)))
	if err := h.renderSessionMarkdown(r.Context(), w, p); err != nil {
		// Headers already sent — cannot signal HTTP error; log for diagnosis.
		slog.Error("conversation export: session render failed mid-stream",
			"event.name", "conversation.export.session_failed", "session_id", p.SessionID, "error", err)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *ConversationHandler) exportSeatZip(w http.ResponseWriter, r *http.Request, p conversation.QueryParams) {
	name := "conversations-" + safeFilename(p.OwnerAccountID)
	if p.StartDate != "" || p.EndDate != "" {
		name += "-" + emptyDash(p.StartDate) + "_" + emptyDash(p.EndDate)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, name))

	zw := zip.NewWriter(w)
	defer zw.Close()

	// Enumerate the seat's sessions (no cap) and render each into its own .md.
	// One failed session is logged and skipped — it must not abort the whole zip.
	err := h.repo.StreamSessionIDs(r.Context(), p, func(sessionID string) error {
		entry, cerr := zw.Create(safeFilename(sessionID) + ".md")
		if cerr != nil {
			return cerr // zip stream is broken — abort
		}
		sp := p
		sp.SessionID = sessionID
		if rerr := h.renderSessionMarkdown(r.Context(), entry, sp); rerr != nil {
			slog.Error("conversation export: session render failed in zip",
				"event.name", "conversation.export.zip_session_failed", "session_id", sessionID, "error", rerr)
		}
		return nil
	})
	if err != nil {
		slog.Error("conversation export: zip enumeration failed",
			"event.name", "conversation.export.zip_failed", "owner_account_id", p.OwnerAccountID, "error", err)
	}
}

// renderSessionMarkdown writes one session as Markdown to w: a header with
// attribution, the once-per-session system prompt, then every turn in order.
func (h *ConversationHandler) renderSessionMarkdown(ctx context.Context, w io.Writer, p conversation.QueryParams) error {
	sys, err := h.repo.SessionSystemText(ctx, p)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "# Conversation — session %s\n\n", p.SessionID)
	fmt.Fprintf(w, "- Owner account: %s\n- Org: %s\n\n", p.OwnerAccountID, p.OrgID)
	if sys != "" {
		fmt.Fprintf(w, "## System prompt\n\n%s\n\n", sys)
	}
	n := 0
	return h.repo.StreamSessionTurns(ctx, p, func(t *conversation.ThreadTurn) error {
		n++
		ts := time.UnixMilli(t.CreatedAt).UTC().Format("2006-01-02 15:04:05 UTC")
		fmt.Fprintf(w, "## Turn %d — %s — %s — %s\n\n", n, ts, dash(t.Model), t.RequestStatus)
		fmt.Fprintf(w, "**User**\n\n%s\n\n", t.UserText)
		fmt.Fprintf(w, "**Assistant**\n\n%s\n\n", t.AssistantText)
		writeToolCalls(w, t.ToolCalls)
		return nil
	})
}

// safeFilename reduces an id to a filesystem/zip-safe token so a crafted
// session/owner id can't escape the entry name. Non [A-Za-z0-9._-] → '_'.
func safeFilename(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func emptyDash(s string) string {
	if s == "" {
		return "all"
	}
	return s
}

// writeToolCalls renders one turn's tool calls into the Markdown export.
//
// 🔴 The export carries them because a page that shows tool calls and an export
// that does not makes the export an INCOMPLETE piece of audit evidence — and
// the export is the artefact that leaves the building, gets attached to a
// ticket, and is read months later by someone who cannot open the console
// (task 13.7d, fence 13.F5).
//
// 🔴 Three renderings, matching the three states the column can hold. The
// middle one is the whole point: an older proxy's turn must NOT read as "no
// tools were called", because nobody established that.
func writeToolCalls(w io.Writer, raw json.RawMessage) {
	if len(raw) == 0 {
		fmt.Fprintf(w, "**Tool calls**\n\n_Not collected — the proxy that captured this turn "+
			"predates tool-call capture. This is not a statement that no tools were called._\n\n")
		return
	}
	var calls []mcpwire.TurnToolCall
	if err := json.Unmarshal(raw, &calls); err != nil {
		// 🔴 Unreadable is its own state, reported. Silently printing "none"
		// would turn a parse failure into a finding of fact.
		fmt.Fprintf(w, "**Tool calls**\n\n_Recorded but unreadable in this build._\n\n")
		return
	}
	if len(calls) == 0 {
		fmt.Fprintf(w, "**Tool calls**\n\n_None._\n\n")
		return
	}
	fmt.Fprintf(w, "**Tool calls**\n\n")
	for i, c := range calls {
		fmt.Fprintf(w, "%d. `%s`", i+1, dash(c.ToolName))
		if c.LinkState != "" {
			fmt.Fprintf(w, " — %s", linkStateWord(c.LinkState))
		}
		fmt.Fprintf(w, "\n")
		// 🔴 The argument SUMMARY, never the values. Same gate as the console:
		// arguments are SQL, file contents and sometimes credentials, and the
		// export is the copy that travels furthest.
		for _, a := range c.ArgsDigest {
			key := a.Key
			if key == "" {
				key = "(value)"
			}
			fmt.Fprintf(w, "   - `%s`: %s, length %d\n", key, a.Type, a.Len)
		}
	}
	fmt.Fprintf(w, "\n")
}

// linkStateWord renders a link state for a human reading an exported file.
//
// 🔴 `pending` and `bypassed` get DIFFERENT sentences and must never be merged:
// the first says "we expect to match this up shortly", the second says "this
// call did not go through AiKey at all", which is a security finding.
func linkStateWord(s mcpwire.LinkState) string {
	switch s {
	case mcpwire.LinkStateLinked:
		return "executed through the gateway"
	case mcpwire.LinkStatePending:
		return "execution record not matched yet"
	case mcpwire.LinkStateBypassed:
		return "NOT ROUTED THROUGH AIKEY — this call bypassed the gateway"
	case mcpwire.LinkStateUnsupported:
		return "the capturing node does not collect execution results"
	default:
		return string(s)
	}
}
