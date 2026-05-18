package memory_test

import (
	"context"
	"errors"
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

	// Non-destructive compaction: the model's view is narrowed to
	// the most recent `maxMessages` (4); the UI's ListMessages
	// still returns all 10 rows.
	modelMsgs, _ := store.GetMessages(ctx, "s")
	if len(modelMsgs) != 4 {
		t.Fatalf("after slide, model view = %d messages, want 4", len(modelMsgs))
	}

	uiMsgs, _ := store.ListMessages(ctx, "s")
	if len(uiMsgs) != 10 {
		t.Fatalf("after slide, UI view = %d messages, want 10 (compaction is non-destructive)", len(uiMsgs))
	}

	// The 6 oldest rows must be flagged hidden; the 4 newest visible.
	hidden, visible := 0, 0
	for _, m := range uiMsgs {
		if m.VisibleToModel {
			visible++
		} else {
			hidden++
		}
	}
	if hidden != 6 || visible != 4 {
		t.Errorf("after slide, hidden=%d visible=%d, want 6 / 4", hidden, visible)
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

	// Fallback hides older rows (no summary row inserted because
	// the model errored). Model sees just the recent half (=2,
	// since summarize halves maxMessages); UI keeps all 8.
	modelMsgs, _ := store.GetMessages(ctx, "s")
	if len(modelMsgs) != 2 {
		t.Fatalf("after fallback slide, model view = %d, want 2 (maxMessages/2)", len(modelMsgs))
	}
	uiMsgs, _ := store.ListMessages(ctx, "s")
	if len(uiMsgs) != 8 {
		t.Fatalf("after fallback slide, UI view = %d, want 8", len(uiMsgs))
	}
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
