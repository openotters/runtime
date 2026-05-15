package agent

import (
	"context"
	"strings"

	"charm.land/fantasy"
	"go.uber.org/zap"

	"github.com/openotters/runtime/pkg/notes"
)

// NotesProvider is the read-only surface BuildNotesPrepareStep
// needs. Declared here so callers can mock the store in tests
// without depending on a real sqlite database.
type NotesProvider interface {
	List(ctx context.Context) ([]notes.Note, error)
}

// BuildNotesPrepareStep returns a fantasy.PrepareStepFunction that
// re-renders the system prompt on every step with a `## Notes`
// section appended to (or prepended in front of) the base system
// prompt.
//
// baseSystem is captured at agent-creation time — it's the
// concatenated context-file blob built by BuildSystemPrompt and
// doesn't change after startup. The notes list is fetched fresh
// on every invocation so newly-saved notes surface to the model
// from the very next step without any external state push.
//
// placement is "above" or "below". Anything else falls through to
// "below". The caller (agent_config.go:setup) is responsible for
// not installing this PrepareStep at all when the operator picked
// "off" — the function never returns a no-op result so the cost of
// installing it when notes are disabled is paid on every step.
//
// Errors fetching notes are NOT fatal. They log a Warn and pass
// the base system prompt through unchanged — notes are decoration,
// never break the turn.
func BuildNotesPrepareStep(
	baseSystem string,
	np NotesProvider,
	placement string,
	logger *zap.Logger,
) fantasy.PrepareStepFunction {
	return func(
		ctx context.Context, _ fantasy.PrepareStepFunctionOptions,
	) (context.Context, fantasy.PrepareStepResult, error) {
		all, err := np.List(ctx)
		if err != nil {
			logger.Warn("notes list failed; falling back to base system prompt", zap.Error(err))
			s := baseSystem
			return ctx, fantasy.PrepareStepResult{System: &s}, nil
		}

		block := notes.RenderPromptSection(all)
		if block == "" {
			// Empty notes list — return base verbatim. Setting
			// System: nil would let fantasy keep its own default,
			// but we explicitly hand it the base so the per-step
			// behaviour is stable regardless of how WithSystemPrompt
			// initialised it.
			s := baseSystem
			return ctx, fantasy.PrepareStepResult{System: &s}, nil
		}

		var b strings.Builder
		if placement == "above" {
			b.WriteString(block)
			b.WriteString("\n---\n\n")
			b.WriteString(baseSystem)
		} else {
			b.WriteString(baseSystem)
			b.WriteString("\n\n---\n\n")
			b.WriteString(block)
		}
		combined := b.String()
		return ctx, fantasy.PrepareStepResult{System: &combined}, nil
	}
}
