package agent_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"go.uber.org/zap"
	_ "modernc.org/sqlite"

	"github.com/openotters/runtime/pkg/agent"
	"github.com/openotters/runtime/pkg/memory"
)

// stubAgent satisfies fantasy.Agent and returns a fixed text response on
// Generate; Stream is unused in these tests but must implement the
// interface.
type stubAgent struct {
	reply string
	err   error
}

func (s *stubAgent) Generate(_ context.Context, _ fantasy.AgentCall) (*fantasy.AgentResult, error) {
	if s.err != nil {
		return nil, s.err
	}

	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{fantasy.TextContent{Text: s.reply}},
		},
	}, nil
}

func (s *stubAgent) Stream(_ context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return s.Generate(context.Background(), fantasy.AgentCall{})
}

func newServiceStore(t *testing.T) *memory.Store {
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

func TestService_ChatRoundTrip(t *testing.T) {
	t.Parallel()

	store := newServiceStore(t)
	svc := agent.NewService(&stubAgent{reply: "pong"}, nil, store, nil, zap.NewNop())

	resp, err := svc.Chat(context.Background(), "s1", "ping")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if resp != "pong" {
		t.Errorf("Chat reply = %q, want pong", resp)
	}

	count, _ := store.CountMessages(context.Background(), "s1")
	if count != 2 {
		t.Errorf("expected 2 stored messages (user+assistant), got %d", count)
	}
}

func TestService_ChatStreamRoundTrip(t *testing.T) {
	t.Parallel()

	store := newServiceStore(t)
	svc := agent.NewService(&stubAgent{reply: "streamed reply"}, nil, store, nil, zap.NewNop())

	var seen []agent.StreamEvent

	resp, err := svc.ChatStream(context.Background(), "stream-s", "ping", func(e agent.StreamEvent) {
		seen = append(seen, e)
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if resp != "streamed reply" {
		t.Errorf("ChatStream resp = %q, want streamed reply", resp)
	}

	count, _ := store.CountMessages(context.Background(), "stream-s")
	if count != 2 {
		t.Errorf("expected user+assistant stored after ChatStream, got %d", count)
	}

	// stubAgent doesn't fire any of the OnStep/OnText/OnTool callbacks,
	// so seen is allowed to be empty — but the field must remain
	// accessible so future stub upgrades are easy. Use it to silence
	// the unused-var lint.
	_ = seen
}

func TestService_ChatStreamPropagatesAgentError(t *testing.T) {
	t.Parallel()

	svc := agent.NewService(
		&stubAgent{err: errors.New("upstream broke")},
		nil, newServiceStore(t), nil, zap.NewNop(),
	)

	_, err := svc.ChatStream(context.Background(), "s", "p", func(_ agent.StreamEvent) {})
	if err == nil || !strings.Contains(err.Error(), "agent stream") {
		t.Fatalf("err = %v, want 'agent stream' wrapping", err)
	}
}

func TestService_ChatPropagatesAgentError(t *testing.T) {
	t.Parallel()

	store := newServiceStore(t)
	svc := agent.NewService(
		&stubAgent{err: errors.New("rate limited")},
		nil, store, nil, zap.NewNop(),
	)

	_, err := svc.Chat(context.Background(), "s2", "ping")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "agent generate") {
		t.Errorf("error %v doesn't wrap agent failure", err)
	}
}

func TestService_PromptObjectNoModelErrors(t *testing.T) {
	t.Parallel()

	svc := agent.NewService(&stubAgent{}, nil, newServiceStore(t), nil, zap.NewNop())

	_, _, err := svc.PromptObject(context.Background(), "p", []byte(`{"type":"object"}`), "n", "d")
	if err == nil || !strings.Contains(err.Error(), "no language model") {
		t.Fatalf("err = %v, want 'no language model'", err)
	}
}

func TestService_PromptObjectEmptySchemaErrors(t *testing.T) {
	t.Parallel()

	svc := agent.NewService(&stubAgent{}, &errorModel{}, newServiceStore(t), nil, zap.NewNop())

	_, _, err := svc.PromptObject(context.Background(), "p", nil, "n", "d")
	if err == nil || !strings.Contains(err.Error(), "schema is required") {
		t.Fatalf("err = %v, want 'schema is required'", err)
	}
}

func TestService_PromptObjectMalformedSchemaJSON(t *testing.T) {
	t.Parallel()

	store := newServiceStore(t)
	svc := agent.NewService(&stubAgent{}, &errorModel{}, store, nil, zap.NewNop())

	_, _, err := svc.PromptObject(context.Background(), "p", []byte("not-json"), "n", "d")
	if err == nil || !strings.Contains(err.Error(), "parsing schema") {
		t.Fatalf("err = %v, want 'parsing schema'", err)
	}
}

func TestService_PromptObjectSchemaMissingTopLevelType(t *testing.T) {
	t.Parallel()

	store := newServiceStore(t)
	svc := agent.NewService(&stubAgent{}, &errorModel{}, store, nil, zap.NewNop())

	_, _, err := svc.PromptObject(context.Background(), "p", []byte(`{"properties":{}}`), "n", "d")
	if err == nil || !strings.Contains(err.Error(), "top-level type") {
		t.Fatalf("err = %v, want 'top-level type'", err)
	}
}

func TestService_ListAndDeleteSession(t *testing.T) {
	t.Parallel()

	store := newServiceStore(t)
	svc := agent.NewService(&stubAgent{reply: "ok"}, nil, store, nil, zap.NewNop())
	ctx := context.Background()

	if _, err := svc.Chat(ctx, "alpha", "first"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, err := svc.Chat(ctx, "beta", "first"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	sessions, err := svc.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}

	msgs, err := svc.ListSessionMessages(ctx, "alpha", 10)
	if err != nil {
		t.Fatalf("ListSessionMessages: %v", err)
	}

	if len(msgs) == 0 || msgs[0].Content != "first" {
		t.Errorf("msgs = %+v, want first user message visible", msgs)
	}

	if err = svc.DeleteSession(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	sessions, _ = svc.ListSessions(ctx)
	if len(sessions) != 1 || sessions[0].ID != "beta" {
		t.Errorf("ListSessions after delete = %+v", sessions)
	}
}

// errorModel implements fantasy.LanguageModel and always errors. Used to
// reach the schema-validation branches of PromptObject without exercising
// the real GenerateObject path.
type errorModel struct{}

func (*errorModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (*errorModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("not used")
}

func (*errorModel) GenerateObject(_ context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (*errorModel) StreamObject(_ context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}

func (*errorModel) Provider() string { return "fake" }
func (*errorModel) Model() string    { return "fake" }
