package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"go.uber.org/zap"

	"github.com/openotters/runtime/pkg/memory"
	"github.com/openotters/runtime/pkg/memoryclient"
)

// stubModel satisfies fantasy.LanguageModel for the summarize path
// and lets tests control whether Generate succeeds or errors.
type stubModel struct {
	reply string
	err   error
}

func (s *stubModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &fantasy.Response{
		Content: fantasy.ResponseContent{fantasy.TextContent{Text: s.reply}},
	}, nil
}

func (s *stubModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("unused in tests")
}

func (*stubModel) GenerateObject(_ context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("unused in tests")
}

func (*stubModel) StreamObject(_ context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("unused in tests")
}

func (*stubModel) Model() string    { return "stub" }
func (*stubModel) Provider() string { return "test" }

// seed inserts n (user, assistant) pairs into sessionID. Every
// caller uses the same sessionID today but the param is kept so the
// helper still reads correctly if a future test needs two sessions.
func seed(t *testing.T, store *memoryclient.Fake, sessionID string, n int) { //nolint:unparam // future-tests friendly
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := store.SaveMessage(ctx, sessionID, "user", "u"); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if _, err := store.SaveMessage(ctx, sessionID, "assistant", "a"); err != nil {
			t.Fatalf("seed assistant: %v", err)
		}
	}
}

func TestCompactor_BelowCapNoOp(t *testing.T) {
	t.Parallel()

	store := memoryclient.NewFake()
	seed(t, store, "s", 2) // 4 rows, cap 10 → no compaction

	c := memory.NewCompactor(memory.Config{
		Strategy: "slide", MaxMessages: 10,
	}, zap.NewNop())

	if err := c.Compact(context.Background(), &stubModel{}, store, "s"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	n, _ := store.CountMessages(context.Background(), "s")
	if n != 4 {
		t.Errorf("below cap should be no-op, got %d rows", n)
	}
}

func TestCompactor_SlideStrategyTrimsTail(t *testing.T) {
	t.Parallel()

	store := memoryclient.NewFake()
	seed(t, store, "s", 10) // 20 rows

	c := memory.NewCompactor(memory.Config{
		Strategy: "slide", MaxMessages: 6,
	}, zap.NewNop())

	if err := c.Compact(context.Background(), &stubModel{}, store, "s"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	n, _ := store.CountMessages(context.Background(), "s")
	if n != 6 {
		t.Errorf("slide kept %d rows, want 6 (cap)", n)
	}
}

func TestCompactor_SummarizePrependsSummary(t *testing.T) {
	t.Parallel()

	store := memoryclient.NewFake()
	seed(t, store, "s", 10) // 20 rows

	c := memory.NewCompactor(memory.Config{
		Strategy: "summarize", MaxMessages: 6,
	}, zap.NewNop())

	if err := c.Compact(context.Background(), &stubModel{reply: "old chat recap"}, store, "s"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	rows, _ := store.ListMessages(context.Background(), "s")
	if len(rows) == 0 || rows[0].Role != "system" {
		t.Fatalf("summary row should lead the survivors, got %+v", rows)
	}
	if !strings.Contains(rows[0].Content, "old chat recap") {
		t.Errorf("summary content = %q, want it to wrap the model reply", rows[0].Content)
	}
}

func TestCompactor_SummarizeFallsBackOnModelError(t *testing.T) {
	t.Parallel()

	store := memoryclient.NewFake()
	seed(t, store, "s", 10)

	c := memory.NewCompactor(memory.Config{
		Strategy: "summarize", MaxMessages: 6,
	}, zap.NewNop())

	// Model errors → fall back to slide (no summary row).
	if err := c.Compact(context.Background(), &stubModel{err: errors.New("rate-limited")}, store, "s"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	rows, _ := store.ListMessages(context.Background(), "s")
	for _, r := range rows {
		if r.Role == "system" {
			t.Errorf("fallback should not insert a summary row, found %+v", r)
		}
	}
}
