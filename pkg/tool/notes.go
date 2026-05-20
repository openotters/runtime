// Tool descriptions are multi-paragraph markdown the model reads in
// its tool catalogue; breaking the prose at 120 chars with Go string
// concatenation hurts readability for reviewers and the rendered
// output the model sees. Same precedent as pkg/tool/agents.go.
//
//nolint:lll // tool descriptions cross the 120-char ceiling intentionally.
package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/openotters/runtime/pkg/notes"
	"github.com/openotters/runtime/pkg/notesclient"
)

// BuildNotesTools returns the six LLM-facing tools that operate on
// the per-agent notes store: note_save, note_list, note_show,
// note_delete, note_pin, note_unpin. The store is shared across all
// six; maxBytes / maxCount are the quota gates the store enforces
// inside Save.
//
// Notes are durable facts the model writes about its operator —
// "the user's k8s cluster is named homelab" — that survive across
// sessions for the lifetime of the agent. They are distinct from
// chat memory (which the compactor may drop or summarise) and from
// context files (which are immutable, baked into the image).
//
// note_pin / note_unpin toggle the in_context flag: pinned notes
// flow into the system prompt on every step via the PrepareStep
// callback, regardless of the notes-prompt-section config. Use it
// for facts the model needs to lean on continuously (project name,
// preferred conventions) rather than facts it only needs to recall
// occasionally (one-off URLs, historical context).
func BuildNotesTools(store notesclient.Store, maxBytes, maxCount int) []fantasy.AgentTool {
	return []fantasy.AgentTool{
		noteSaveTool(store, maxBytes, maxCount),
		noteListTool(store),
		noteShowTool(store),
		noteDeleteTool(store),
		notePinTool(store),
		noteUnpinTool(store),
	}
}

type noteSaveInput struct {
	Key     string `json:"key"     jsonschema:"description=Note key: [a-z0-9_-], <=64 chars. Re-using overwrites."`
	Content string `json:"content" jsonschema:"description=Free-form body. Capped by notes-max-bytes-per."`
	Pin     bool   `json:"pin,omitempty" jsonschema:"description=When true, also pin (= note_pin) so the note renders in your system prompt on every step."`
}

type noteKeyInput struct {
	Key string `json:"key" jsonschema:"description=Note identifier as returned by note_list."`
}

func noteSaveTool(store notesclient.Store, maxBytes, maxCount int) fantasy.AgentTool {
	desc := "**Save this whenever the user states a durable fact** — " +
		"cluster names, kubeconfig paths, preferred conventions, " +
		"environment variable locations, project conventions, anything " +
		"you'll want to recall in a future session. Notes persist across " +
		"sessions; chat history does not. If you don't save now, you'll " +
		"forget next time. Re-using a key OVERWRITES the existing note; " +
		"call note_list first if unsure whether a key is taken.\n\n" +
		"Pass `pin: true` to save AND pin in one call — the note then " +
		"renders in your system prompt on every subsequent step. Use " +
		"for facts you want visible on every future turn (e.g. \"this " +
		"user's k8s cluster is named homelab\")."
	return fantasy.NewAgentTool(
		"note_save",
		desc,
		func(ctx context.Context, in noteSaveInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			key := strings.TrimSpace(in.Key)
			if key == "" {
				return fantasy.ToolResponse{
					IsError: true,
					Content: "key is required (e.g. note_save k8s-cluster ...)",
				}, nil
			}

			// Probe existence before Save so the response can tell
			// the model "new" vs "overwrote prior" without an extra
			// round-trip. A race window exists (two concurrent
			// note_save with same key) but the agent is single-
			// threaded so this can't actually happen today.
			_, getErr := store.Get(ctx, key)
			existed := getErr == nil

			if err := store.Save(ctx, key, in.Content, maxBytes, maxCount); err != nil {
				return errToolResp(err)
			}

			// Pin in the same call. SetInContext is idempotent —
			// re-pinning a pinned note is a no-op. Treat a pin
			// failure as a hard error: the model asked for it
			// explicitly, so silently dropping it would surprise.
			if in.Pin {
				if err := store.SetInContext(ctx, key, true); err != nil {
					return errToolResp(err)
				}
			}

			pinSuffix := ""
			if in.Pin {
				pinSuffix = " (pinned)"
			}
			if existed {
				return fantasy.ToolResponse{
					Content: fmt.Sprintf("saved (overwrote prior version of %q)%s", key, pinSuffix),
				}, nil
			}
			return fantasy.ToolResponse{
				Content: fmt.Sprintf("saved (new key %q)%s", key, pinSuffix),
			}, nil
		},
	)
}

func noteListTool(store notesclient.Store) fantasy.AgentTool {
	desc := "**Run this at the start of any non-trivial task** — and " +
		"before asking the user a question whose answer you might " +
		"have already saved. Returns key + last-updated + preview for " +
		"every note. Use note_show for the full body once you know " +
		"the key you want."
	return fantasy.NewAgentTool(
		"note_list",
		desc,
		func(ctx context.Context, _ emptyInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			all, err := store.List(ctx)
			if err != nil {
				return errToolResp(err)
			}
			if len(all) == 0 {
				return fantasy.ToolResponse{Content: "No notes stored.\n"}, nil
			}
			return fantasy.ToolResponse{Content: notes.RenderPromptSection(all)}, nil
		},
	)
}

func noteShowTool(store notesclient.Store) fantasy.AgentTool {
	desc := "Read the full body of a saved note. Use after note_list " +
		"to load the specific fact you need — previews are truncated, " +
		"this is the source of truth. Errors with the list of " +
		"available keys when the requested key doesn't exist."
	return fantasy.NewAgentTool(
		"note_show",
		desc,
		func(ctx context.Context, in noteKeyInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			key := strings.TrimSpace(in.Key)
			if key == "" {
				return fantasy.ToolResponse{
					IsError: true,
					Content: "key is required (e.g. note_show k8s-cluster)",
				}, nil
			}

			n, err := store.Get(ctx, key)
			if errors.Is(err, notesclient.ErrNoteNotFound) {
				return fantasy.ToolResponse{
					IsError: true,
					Content: missingKeyHint(ctx, store, key),
				}, nil
			}
			if err != nil {
				return errToolResp(err)
			}
			return fantasy.ToolResponse{Content: n.Content}, nil
		},
	)
}

func noteDeleteTool(store notesclient.Store) fantasy.AgentTool {
	desc := "Drop a saved note when the user retracts the fact, " +
		"corrects it, or it's no longer accurate. Idempotent: " +
		"deleting a missing key still succeeds (the response " +
		"distinguishes the two cases). Pair with note_save when " +
		"replacing a fact wholesale — overwriting an existing key " +
		"via note_save is usually faster than delete+save."
	return fantasy.NewAgentTool(
		"note_delete",
		desc,
		func(ctx context.Context, in noteKeyInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			key := strings.TrimSpace(in.Key)
			if key == "" {
				return fantasy.ToolResponse{
					IsError: true,
					Content: "key is required (e.g. note_delete k8s-cluster)",
				}, nil
			}

			_, getErr := store.Get(ctx, key)
			existed := getErr == nil

			if err := store.Delete(ctx, key); err != nil {
				return errToolResp(err)
			}

			if existed {
				return fantasy.ToolResponse{
					Content: fmt.Sprintf("deleted %q", key),
				}, nil
			}
			return fantasy.ToolResponse{
				Content: fmt.Sprintf("no note named %q (already absent)", key),
			}, nil
		},
	)
}

func notePinTool(store notesclient.Store) fantasy.AgentTool {
	desc := "**Pin a note when you'll reference it on every step** of " +
		"the current task — target cluster, active project, deployment " +
		"invariant. Pinned notes flow into your system prompt as " +
		"full-content blocks automatically, so you stop burning tool " +
		"calls re-reading them. When the task is done, note_unpin to " +
		"keep the pinned set tight (token budget). Pinning a missing " +
		"key errors with the available keys."
	return fantasy.NewAgentTool(
		"note_pin",
		desc,
		func(ctx context.Context, in noteKeyInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return setInContextResp(ctx, store, in.Key, true)
		},
	)
}

func noteUnpinTool(store notesclient.Store) fantasy.AgentTool {
	desc := "Unpin when a note is no longer load-bearing for the " +
		"current task — keep the pinned set tight so the prompt " +
		"stays focused. The note stays saved (note_show still works); " +
		"only automatic prompt-inclusion is cleared. Idempotent: " +
		"unpinning an already-unpinned note succeeds."
	return fantasy.NewAgentTool(
		"note_unpin",
		desc,
		func(ctx context.Context, in noteKeyInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return setInContextResp(ctx, store, in.Key, false)
		},
	)
}

// setInContextResp shares the body of note_pin and note_unpin: both
// validate the key, call SetInContext, and map sql.ErrNoRows into a
// missing-key hint for the model. The only behavioural difference
// between the two tools is the bool they pass through.
func setInContextResp(
	ctx context.Context, store notesclient.Store, key string, inContext bool,
) (fantasy.ToolResponse, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		verb := "note_pin"
		if !inContext {
			verb = "note_unpin"
		}
		return fantasy.ToolResponse{
			IsError: true,
			Content: fmt.Sprintf("key is required (e.g. %s k8s-cluster)", verb),
		}, nil
	}

	err := store.SetInContext(ctx, key, inContext)
	if errors.Is(err, notesclient.ErrNoteNotFound) {
		return fantasy.ToolResponse{
			IsError: true,
			Content: missingKeyHint(ctx, store, key),
		}, nil
	}
	if err != nil {
		return errToolResp(err)
	}

	if inContext {
		return fantasy.ToolResponse{
			Content: fmt.Sprintf("pinned %q (now in system prompt on every step)", key),
		}, nil
	}
	return fantasy.ToolResponse{
		Content: fmt.Sprintf("unpinned %q (no longer in system prompt)", key),
	}, nil
}

// missingKeyHint builds the IsError message for note_show on a
// missing key — same UX pattern as context_show: tell the model
// which keys actually exist so it can pick the right one without
// guessing or hallucinating.
func missingKeyHint(ctx context.Context, store notesclient.Store, key string) string {
	all, err := store.List(ctx)
	if err != nil || len(all) == 0 {
		return fmt.Sprintf("no note named %q (no notes stored)", key)
	}

	const maxKeys = 20
	keys := make([]string, 0, len(all))
	for _, n := range all {
		keys = append(keys, n.Key)
		if len(keys) == maxKeys {
			break
		}
	}
	trailer := ""
	if len(all) > maxKeys {
		trailer = fmt.Sprintf(" (+%d more — use note_list to see all)", len(all)-maxKeys)
	}
	return fmt.Sprintf("no note named %q; stored: %s%s", key, strings.Join(keys, ", "), trailer)
}
