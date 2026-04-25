package memory_test

import (
	"context"
	"database/sql"
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
