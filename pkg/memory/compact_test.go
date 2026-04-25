package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"go.uber.org/zap"
	_ "modernc.org/sqlite"

	"github.com/openotters/runtime/pkg/memory"
)

func TestCompactor_NoOpWhenUnderLimits(t *testing.T) {
	t.Parallel()

	c := memory.NewCompactor(memory.Config{
		Strategy: "sliding", MaxMessages: 100,
	}, zap.NewNop())

	store := newTestStore(t)
	ctx := context.Background()

	for range 5 {
		if err := store.SaveMessage(ctx, "s", "user", "msg"); err != nil {
			t.Fatalf("SaveMessage: %v", err)
		}
	}

	if err := c.Compact(ctx, nil, store, "s"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	count, _ := store.CountMessages(ctx, "s")
	if count != 5 {
		t.Errorf("count after no-op compact = %d, want 5", count)
	}
}

func TestCompactor_SlidingDropsOldMessages(t *testing.T) {
	t.Parallel()

	c := memory.NewCompactor(memory.Config{
		Strategy: "sliding", MaxMessages: 4,
	}, zap.NewNop())

	store := newTestStore(t)
	ctx := context.Background()

	// 10 messages > maxMessages=4 → triggers slide.
	for i := range 10 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}

		if err := store.SaveMessage(ctx, "s", role, "msg"); err != nil {
			t.Fatalf("SaveMessage: %v", err)
		}
	}

	if err := c.Compact(ctx, nil, store, "s"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.GetMessages(ctx, "s")

	// kept = maxMessages (4) + 1 system notice = 5
	if len(msgs) != 5 {
		t.Fatalf("after slide, got %d messages, want 5", len(msgs))
	}

	last := lastTextPart(t, msgs)
	if !strings.Contains(last, "Memory compacted") {
		t.Errorf("missing system notice; last message = %q", last)
	}
}

func TestCompactor_SummarizeFallsBackToSlideOnModelError(t *testing.T) {
	t.Parallel()

	c := memory.NewCompactor(memory.Config{
		Strategy: "summarize", MaxMessages: 4,
	}, zap.NewNop())

	store := newTestStore(t)
	ctx := context.Background()

	for i := range 8 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}

		if err := store.SaveMessage(ctx, "s", role, "msg"); err != nil {
			t.Fatalf("SaveMessage: %v", err)
		}
	}

	model := &errorLM{}

	if err := c.Compact(ctx, model, store, "s"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.GetMessages(ctx, "s")

	// On model error the compactor falls back to sliding.
	last := lastTextPart(t, msgs)
	if !strings.Contains(last, "sliding") {
		t.Errorf("expected sliding-fallback notice; got %q", last)
	}
}

func lastTextPart(t *testing.T, msgs []fantasy.Message) string {
	t.Helper()

	if len(msgs) == 0 {
		t.Fatal("messages empty")
	}

	last := msgs[len(msgs)-1]
	if len(last.Content) == 0 {
		t.Fatal("last message has no parts")
	}

	tp, ok := last.Content[0].(fantasy.TextPart)
	if !ok {
		t.Fatalf("last message part type = %T, want TextPart", last.Content[0])
	}

	return tp.Text
}

// errorLM is the smallest fantasy.LanguageModel that always errors on
// Generate — enough to exercise the summarize→slide fallback.
type errorLM struct{}

func (*errorLM) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return nil, errFakeModel
}

func (*errorLM) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errFakeModel
}

func (*errorLM) GenerateObject(_ context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errFakeModel
}

func (*errorLM) StreamObject(_ context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errFakeModel
}

func (*errorLM) Provider() string { return "fake" }
func (*errorLM) Model() string    { return "fake-1" }

var errFakeModel = errors.New("fake model error")
