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
type stubNotesProvider struct {
	out []notes.Note
	err error
}

func (s *stubNotesProvider) List(_ context.Context) ([]notes.Note, error) {
	return s.out, s.err
}

func TestBuildNotesPrepareStep_InjectsBelow(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{out: []notes.Note{
		{Key: "k8s-cluster", Preview: "homelab", UpdatedAt: time.Now()},
	}}

	fn := agent.BuildNotesPrepareStep(base, provider, "below", zap.NewNop())
	_, res, err := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})
	if err != nil {
		t.Fatalf("prepare-step errored: %v", err)
	}
	if res.System == nil {
		t.Fatal("System pointer is nil")
	}

	got := *res.System
	// Base must appear first, then the notes block; the model reads
	// "you are this agent" before "and here is the user's KV".
	idxBase := strings.Index(got, "## AGENT")
	idxNotes := strings.Index(got, "## Notes")
	if idxBase < 0 || idxNotes < 0 {
		t.Fatalf("missing markers in output:\n%s", got)
	}
	if idxBase > idxNotes {
		t.Errorf("expected base before notes; got base at %d, notes at %d", idxBase, idxNotes)
	}
	if !strings.Contains(got, "`k8s-cluster`") {
		t.Errorf("note key missing in output:\n%s", got)
	}
}

func TestBuildNotesPrepareStep_InjectsAbove(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{out: []notes.Note{
		{Key: "k", Preview: "v", UpdatedAt: time.Now()},
	}}

	fn := agent.BuildNotesPrepareStep(base, provider, "above", zap.NewNop())
	_, res, _ := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})

	got := *res.System
	idxBase := strings.Index(got, "## AGENT")
	idxNotes := strings.Index(got, "## Notes")
	if idxNotes > idxBase {
		t.Errorf("'above' should put notes first; got base at %d, notes at %d", idxBase, idxNotes)
	}
}

func TestBuildNotesPrepareStep_EmptyListPassesBaseThrough(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{out: nil}

	fn := agent.BuildNotesPrepareStep(base, provider, "below", zap.NewNop())
	_, res, _ := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})

	if *res.System != base {
		t.Errorf("empty notes list should pass base unchanged.\n got  %q\n want %q", *res.System, base)
	}
}

func TestBuildNotesPrepareStep_ProviderErrorSoftFails(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{err: errors.New("boom")}

	fn := agent.BuildNotesPrepareStep(base, provider, "below", zap.NewNop())
	_, res, err := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})
	if err != nil {
		t.Fatalf("provider error must not propagate (turn would die); got %v", err)
	}
	if *res.System != base {
		t.Errorf("provider error should pass base unchanged; got %q", *res.System)
	}
}

func TestBuildNotesPrepareStep_UnknownPlacementDefaultsBelow(t *testing.T) {
	t.Parallel()

	base := "## AGENT\nbody"
	provider := &stubNotesProvider{out: []notes.Note{
		{Key: "k", Preview: "v", UpdatedAt: time.Now()},
	}}

	fn := agent.BuildNotesPrepareStep(base, provider, "sideways", zap.NewNop())
	_, res, _ := fn(context.Background(), fantasy.PrepareStepFunctionOptions{})

	idxBase := strings.Index(*res.System, "## AGENT")
	idxNotes := strings.Index(*res.System, "## Notes")
	if idxNotes < idxBase {
		t.Errorf("unknown placement should fall back to below; got base at %d, notes at %d", idxBase, idxNotes)
	}
}
