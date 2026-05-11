package memory_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"charm.land/fantasy"
	_ "modernc.org/sqlite"

	"github.com/openotters/runtime/pkg/memory"
)

func newTestStore(t *testing.T) *memory.Store {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	store, err := memory.NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	return store
}

func TestStore_SaveAndGetMessages(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	if err := store.SaveMessage(ctx, "s1", "user", "hello"); err != nil {
		t.Fatalf("SaveMessage user: %v", err)
	}

	if err := store.SaveMessage(ctx, "s1", "assistant", "hi there"); err != nil {
		t.Fatalf("SaveMessage assistant: %v", err)
	}

	msgs, err := store.GetMessages(ctx, "s1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}

	if msgs[0].Role != fantasy.MessageRoleUser {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}

	if msgs[1].Role != fantasy.MessageRoleAssistant {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}
}

// TestStore_GetMessagesPreservesToolHistory pins the contract that
// stored assistant turns surface their ToolCallPart + ToolResultPart
// history when replayed for the LLM. Regression for the bug where
// `flattenAssistantParts` quietly dropped every `tool` part, so the
// model couldn't recall a job_id it had just submitted (it saw only
// its own narration text, never the tool call/result graph).
func TestStore_GetMessagesPreservesToolHistory(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	if err := store.SaveMessage(ctx, "s", "user", "submit a job"); err != nil {
		t.Fatalf("SaveMessage user: %v", err)
	}

	// Mimic the agent service's stored assistant shape: one text
	// part followed by one completed tool call (input + output +
	// state=output-available).
	assistantContent := `[
		{"kind":"text","text":"submitting…"},
		{"kind":"tool","tool_id":"call_1","name":"job_submit",
		 "input":"{\"bin\":\"yaegi\"}","output":"{\"job_id\":\"job_abc\"}",
		 "state":"output-available"}
	]`
	if err := store.SaveMessage(ctx, "s", "assistant", assistantContent); err != nil {
		t.Fatalf("SaveMessage assistant: %v", err)
	}

	msgs, err := store.GetMessages(ctx, "s")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	// Expect: user, assistant (text + ToolCallPart), tool (ToolResultPart).
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3 (user, assistant, tool):\n%+v", len(msgs), msgs)
	}

	if msgs[1].Role != fantasy.MessageRoleAssistant {
		t.Fatalf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}

	var foundCall bool
	for _, p := range msgs[1].Content {
		if tc, ok := p.(fantasy.ToolCallPart); ok &&
			tc.ToolCallID == "call_1" && tc.ToolName == "job_submit" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Fatalf("assistant message missing ToolCallPart for call_1:\n%+v", msgs[1])
	}

	if msgs[2].Role != fantasy.MessageRoleTool {
		t.Fatalf("msgs[2].Role = %q, want tool", msgs[2].Role)
	}

	var foundResult bool
	for _, p := range msgs[2].Content {
		tr, ok := p.(fantasy.ToolResultPart)
		if !ok || tr.ToolCallID != "call_1" {
			continue
		}
		text, ok := tr.Output.(fantasy.ToolResultOutputContentText)
		if !ok {
			continue
		}
		if text.Text != `{"job_id":"job_abc"}` {
			t.Errorf("ToolResultPart.Output = %q, want job_abc payload", text.Text)
		}
		foundResult = true
	}
	if !foundResult {
		t.Fatalf("tool message missing ToolResultPart for call_1:\n%+v", msgs[2])
	}
}

// TestStore_GetMessagesInterruptedToolGetsSyntheticResult locks the
// invariant that a tool call without a captured result still emits
// a ToolResultPart (synthetic "(interrupted)"). Providers like
// Anthropic reject conversations where a tool_use has no matching
// tool_result, so a cancelled mid-call turn would otherwise refuse
// to resume.
func TestStore_GetMessagesInterruptedToolGetsSyntheticResult(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	assistantContent := `[
		{"kind":"tool","tool_id":"call_x","name":"job_wait",
		 "input":"{\"job_id\":\"job_abc\"}","state":"input-available"}
	]`
	if err := store.SaveMessage(ctx, "s", "assistant", assistantContent); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	msgs, err := store.GetMessages(ctx, "s")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2 (assistant, tool):\n%+v", len(msgs), msgs)
	}

	tr, ok := msgs[1].Content[0].(fantasy.ToolResultPart)
	if !ok {
		t.Fatalf("expected ToolResultPart, got %T", msgs[1].Content[0])
	}

	text, ok := tr.Output.(fantasy.ToolResultOutputContentText)
	if !ok {
		t.Fatalf("expected ToolResultOutputContentText, got %T", tr.Output)
	}

	if !strings.Contains(text.Text, "interrupted") {
		t.Errorf("synthetic result text = %q, want 'interrupted' marker", text.Text)
	}
}

// TestStore_GetMessagesSplitsTextAfterTool pins the multi-step
// reconstruction: a stored assistant row containing
// text → tool → text MUST replay as
//
//	assistant 1: [text, tool_call]
//	tool:        [tool_result]
//	assistant 2: [text]
//
// Anthropic rejects (with a 400 invalid_request_error) any
// conversation where a tool_use is followed by more content
// inside the same assistant message — the post-tool text belongs
// to a NEW model step. Regression for the bug where every stored
// turn collapsed into one assistant + one tool message, sending
// `assistant: [text, tool_use, text]` to the API.
func TestStore_GetMessagesSplitsTextAfterTool(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	assistantContent := `[
		{"kind":"text","text":"I'll check."},
		{"kind":"tool","tool_id":"toolu_1","name":"ha",
		 "input":"{\"args\":[\"info\"]}","output":"sup info","state":"output-available"},
		{"kind":"text","text":"Done — info looks healthy."}
	]`
	if err := store.SaveMessage(ctx, "s", "assistant", assistantContent); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	msgs, err := store.GetMessages(ctx, "s")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3 (assistant, tool, assistant):\n%+v", len(msgs), msgs)
	}

	if msgs[0].Role != fantasy.MessageRoleAssistant ||
		msgs[1].Role != fantasy.MessageRoleTool ||
		msgs[2].Role != fantasy.MessageRoleAssistant {
		t.Fatalf("roles = %v / %v / %v, want assistant/tool/assistant",
			msgs[0].Role, msgs[1].Role, msgs[2].Role)
	}

	// First assistant carries the pre-tool text + the tool call.
	if len(msgs[0].Content) != 2 {
		t.Fatalf("msgs[0].Content len = %d, want 2", len(msgs[0].Content))
	}
	if _, ok := msgs[0].Content[0].(fantasy.TextPart); !ok {
		t.Errorf("msgs[0].Content[0] = %T, want TextPart", msgs[0].Content[0])
	}
	if _, ok := msgs[0].Content[1].(fantasy.ToolCallPart); !ok {
		t.Errorf("msgs[0].Content[1] = %T, want ToolCallPart", msgs[0].Content[1])
	}

	// Tool message holds exactly the matching result.
	if len(msgs[1].Content) != 1 {
		t.Fatalf("tool content len = %d, want 1", len(msgs[1].Content))
	}

	// Second assistant carries only the post-tool text.
	if len(msgs[2].Content) != 1 {
		t.Fatalf("msgs[2].Content len = %d, want 1", len(msgs[2].Content))
	}
	tp, ok := msgs[2].Content[0].(fantasy.TextPart)
	if !ok {
		t.Fatalf("msgs[2].Content[0] = %T, want TextPart", msgs[2].Content[0])
	}
	if !strings.Contains(tp.Text, "Done") {
		t.Errorf("msgs[2] text = %q, want post-tool text", tp.Text)
	}
}

func TestStore_GetMessagesEmpty(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	msgs, err := store.GetMessages(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	if len(msgs) != 0 {
		t.Fatalf("got %d messages, want 0", len(msgs))
	}
}

func TestStore_CountMessages(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	for range 3 {
		if err := store.SaveMessage(ctx, "s2", "user", "msg"); err != nil {
			t.Fatalf("SaveMessage: %v", err)
		}
	}

	count, err := store.CountMessages(ctx, "s2")
	if err != nil {
		t.Fatalf("CountMessages: %v", err)
	}

	if count != 3 {
		t.Errorf("CountMessages = %d, want 3", count)
	}
}

func TestStore_ReplaceMessages(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	for _, content := range []string{"first", "second", "third"} {
		if err := store.SaveMessage(ctx, "s3", "user", content); err != nil {
			t.Fatalf("SaveMessage: %v", err)
		}
	}

	replacement := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "summary"}}},
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "next"}}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: ""}}}, // dropped
	}

	if err := store.ReplaceMessages(ctx, "s3", replacement); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	count, _ := store.CountMessages(ctx, "s3")
	if count != 2 {
		t.Fatalf("CountMessages after replace = %d, want 2 (empty body skipped)", count)
	}

	msgs, _ := store.GetMessages(ctx, "s3")

	tp, ok := msgs[0].Content[0].(fantasy.TextPart)
	if !ok {
		t.Fatalf("msgs[0] part type = %T, want TextPart", msgs[0].Content[0])
	}

	if tp.Text != "summary" {
		t.Errorf("msgs[0] = %q, want summary", tp.Text)
	}
}

func TestStore_ListSessionsAndDelete(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	for _, sid := range []string{"a", "b"} {
		if err := store.SaveMessage(ctx, sid, "user", "x"); err != nil {
			t.Fatalf("SaveMessage: %v", err)
		}
	}

	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}

	if delErr := store.DeleteSession(ctx, "a"); delErr != nil {
		t.Fatalf("DeleteSession: %v", delErr)
	}

	count, _ := store.CountMessages(ctx, "a")
	if count != 0 {
		t.Errorf("after DeleteSession, count = %d, want 0", count)
	}

	sessions, _ = store.ListSessions(ctx)
	if len(sessions) != 1 || sessions[0].ID != "b" {
		t.Errorf("ListSessions after delete = %+v, want [{ID:b}]", sessions)
	}
}
