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
// without depending on a real sqlite database. Two queries — one
// for "every note" (preview-table rendering) and one for "only
// pinned notes" (full-content block rendering) — keep the
// PrepareStep callback from doing client-side filtering.
type NotesProvider interface {
	List(ctx context.Context) ([]notes.Note, error)
	ListInContext(ctx context.Context) ([]notes.Note, error)
}

// BuildNotesPrepareStep returns a fantasy.PrepareStepFunction that
// re-renders the system prompt on every step with two optional
// blocks merged into the base:
//
//  1. A preview table of ALL notes — only when section is "above"
//     or "below"; "off" suppresses it. Controls global visibility.
//  2. A pinned-notes section with full content — always rendered
//     when there are notes flagged in_context, regardless of
//     section. This is the per-note opt-in the model uses for facts
//     it expects to lean on continuously.
//
// baseSystem is captured at agent-creation time. The two note
// queries are issued fresh on every step so newly-saved or
// newly-pinned notes surface to the model from the very next step
// without any external state push.
//
// section is "off" (default), "above", or "below". Anything else is
// treated as "below" for the table. The pinned block always
// appears immediately after the table when both exist; when the
// table is suppressed, the pinned block goes below the base.
//
// Errors fetching notes are NOT fatal. They log a Warn and pass
// the base system prompt through unchanged — notes are decoration,
// never break the turn.
func BuildNotesPrepareStep(
	baseSystem string,
	np NotesProvider,
	section string,
	logger *zap.Logger,
) fantasy.PrepareStepFunction {
	return func(
		ctx context.Context, _ fantasy.PrepareStepFunctionOptions,
	) (context.Context, fantasy.PrepareStepResult, error) {
		pinned, pErr := np.ListInContext(ctx)
		if pErr != nil {
			logger.Warn("pinned notes list failed", zap.Error(pErr))
			pinned = nil
		}

		var all []notes.Note
		if section == "above" || section == "below" {
			var aErr error
			all, aErr = np.List(ctx)
			if aErr != nil {
				logger.Warn("notes list failed; suppressing preview table", zap.Error(aErr))
				all = nil
			}
		}

		tableBlock := notes.RenderPromptSection(all)
		pinnedBlock := notes.RenderInContextBlock(pinned)

		// Fast path: nothing to render — return the base prompt
		// verbatim so the model sees a stable system prompt across
		// turns when the notes feature is dormant.
		if tableBlock == "" && pinnedBlock == "" {
			s := baseSystem
			return ctx, fantasy.PrepareStepResult{System: &s}, nil
		}

		combined := composePrompt(baseSystem, section, tableBlock, pinnedBlock)
		return ctx, fantasy.PrepareStepResult{System: &combined}, nil
	}
}

// composePrompt stitches the base prompt and the two optional note
// blocks into the per-step system prompt. The pinned block always
// follows the preview table (when both exist) so the model reads
// "all your notes look like this" → "and these specific ones you
// need verbatim". When section is "off" the preview table is
// suppressed; the pinned block (if any) goes below the base.
func composePrompt(base, section, table, pinnedBlock string) string {
	var notesPart strings.Builder
	if table != "" {
		notesPart.WriteString(table)
	}
	if pinnedBlock != "" {
		if notesPart.Len() > 0 {
			notesPart.WriteString("\n")
		}
		notesPart.WriteString(pinnedBlock)
	}

	var out strings.Builder
	if section == "above" {
		out.WriteString(notesPart.String())
		out.WriteString("\n---\n\n")
		out.WriteString(base)
	} else {
		out.WriteString(base)
		out.WriteString("\n\n---\n\n")
		out.WriteString(notesPart.String())
	}
	return out.String()
}
