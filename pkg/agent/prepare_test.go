package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"go.uber.org/zap"

	"github.com/openotters/runtime/pkg/agent"
	"github.com/openotters/runtime/pkg/notes"
)

// stubNotesProvider lets the prepare-step tests drive the
// PrepareStepFunction without standing up a real sqlite database.
// Both queries can be primed independently so each test can
// exercise the table block, the pinned block, or both.
type stubNotesProvider struct {
	all       []notes.Note
	allErr    error
	pinned    []notes.Note
	pinnedErr error
}

func (s *stubNotesProvider) List(_ context.Context) ([]notes.Note, error) {
	return s.all, s.allErr
}

func (s *stubNotesProvider) ListInContext(_ context.Context) ([]notes.Note, error) {
	return s.pinned, s.pinnedErr
}

func TestBuildNotesPrepareStep_PinnedBlockAlwaysRenders(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	// Section is "off" — the preview table is suppressed, but the
	// pinned block must still flow in. This is the load-bearing
	// guarantee of the in_context flag: pinning works without
	// flipping notes-prompt-section.
	provider := &stubNotesProvider{
		pinned: []notes.Note{
			{Key: "cluster", Content: "homelab, 3 nodes", InContext: true, UpdatedAt: time.Now()},
		},
	}

	fn := agent.BuildNotesPrepareStep(base, provider, "off", zap.NewNop())
	_, res, err := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})
	if err != nil {
		t.Fatalf("prepare-step errored: %v", err)
	}
	got := *res.System
	if !strings.Contains(got, "## Pinned notes") {
		t.Errorf("missing 'Pinned notes' header:\n%s", got)
	}
	if !strings.Contains(got, "### cluster") {
		t.Errorf("missing per-key subheader:\n%s", got)
	}
	if !strings.Contains(got, "homelab, 3 nodes") {
		t.Errorf("full content not rendered:\n%s", got)
	}
	if strings.Contains(got, "| Key | Updated | Preview |") {
		t.Errorf("preview table should be suppressed when section=off:\n%s", got)
	}
}

func TestBuildNotesPrepareStep_SectionAdditiveToPinned(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{
		all: []notes.Note{
			{Key: "a", Preview: "first", UpdatedAt: time.Now()},
			{Key: "b", Preview: "second", UpdatedAt: time.Now()},
		},
		pinned: []notes.Note{
			{Key: "b", Content: "second", InContext: true, UpdatedAt: time.Now()},
		},
	}

	fn := agent.BuildNotesPrepareStep(base, provider, "below", zap.NewNop())
	_, res, _ := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})
	got := *res.System

	idxTable := strings.Index(got, "## Notes")
	idxPinned := strings.Index(got, "## Pinned notes")
	if idxTable < 0 || idxPinned < 0 {
		t.Fatalf("expected both blocks; got:\n%s", got)
	}
	// Pinned block follows the preview table when both exist —
	// model reads "everything you have" → "and these specifically".
	if idxTable > idxPinned {
		t.Errorf("preview table should precede pinned block; got table at %d, pinned at %d",
			idxTable, idxPinned)
	}
}

func TestBuildNotesPrepareStep_NothingRendersWhenEmpty(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{} // no all, no pinned

	fn := agent.BuildNotesPrepareStep(base, provider, "below", zap.NewNop())
	_, res, _ := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})

	if *res.System != base {
		t.Errorf("no notes → expected base prompt unchanged:\n got %q\n want %q",
			*res.System, base)
	}
}

func TestBuildNotesPrepareStep_SectionAboveOrdering(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{
		all: []notes.Note{{Key: "k", Preview: "v", UpdatedAt: time.Now()}},
	}

	fn := agent.BuildNotesPrepareStep(base, provider, "above", zap.NewNop())
	_, res, _ := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})

	got := *res.System
	idxBase := strings.Index(got, "## AGENT")
	idxNotes := strings.Index(got, "## Notes")
	if idxNotes > idxBase {
		t.Errorf("'above' should put notes first; got base at %d, notes at %d",
			idxBase, idxNotes)
	}
}

func TestBuildNotesPrepareStep_PinnedErrorSoftFails(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{pinnedErr: errors.New("boom")}

	fn := agent.BuildNotesPrepareStep(base, provider, "off", zap.NewNop())
	_, res, err := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})
	if err != nil {
		t.Fatalf("pinned error must not propagate; got %v", err)
	}
	if *res.System != base {
		t.Errorf("pinned error should pass base unchanged; got %q", *res.System)
	}
}

func TestBuildNotesPrepareStep_AllErrorWhenSectionOnSoftFails(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{
		allErr: errors.New("boom"),
		pinned: []notes.Note{
			{Key: "k", Content: "v", InContext: true, UpdatedAt: time.Now()},
		},
	}

	// section "below" requests the table; if the all-query fails
	// the runtime should still render the pinned block — the two
	// queries are independent.
	fn := agent.BuildNotesPrepareStep(base, provider, "below", zap.NewNop())
	_, res, _ := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})

	got := *res.System
	if strings.Contains(got, "## Notes") {
		t.Errorf("preview table should be suppressed on all-error:\n%s", got)
	}
	if !strings.Contains(got, "## Pinned notes") {
		t.Errorf("pinned block should still render despite all-error:\n%s", got)
	}
}
