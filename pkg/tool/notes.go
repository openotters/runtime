package tool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/openotters/runtime/pkg/notes"
)

// BuildNotesTools returns the four LLM-facing tools that operate on
// the per-agent notes store: note_save, note_list, note_show,
// note_delete. The store is shared across all four; maxBytes /
// maxCount are the quota gates the store enforces inside Save.
//
// Notes are durable facts the model writes about its operator —
// "the user's k8s cluster is named homelab" — that survive across
// sessions for the lifetime of the agent. They are distinct from
// chat memory (which the compactor may drop or summarise) and from
// context files (which are immutable, baked into the image).
func BuildNotesTools(store *notes.Store, maxBytes, maxCount int) []fantasy.AgentTool {
	return []fantasy.AgentTool{
		noteSaveTool(store, maxBytes, maxCount),
		noteListTool(store),
		noteShowTool(store),
		noteDeleteTool(store),
	}
}

type noteSaveInput struct {
	Key     string `json:"key"     jsonschema:"description=Note key: [a-z0-9_-], <=64 chars. Re-using overwrites."`
	Content string `json:"content" jsonschema:"description=Free-form body. Capped by notes-max-bytes-per."`
}

type noteKeyInput struct {
	Key string `json:"key" jsonschema:"description=Note identifier as returned by note_list."`
}

func noteSaveTool(store *notes.Store, maxBytes, maxCount int) fantasy.AgentTool {
	desc := "Save a durable fact under a short key. Notes persist " +
		"across sessions for the lifetime of this agent — use them for " +
		"things the user expects you to remember (cluster names, " +
		"preferred conventions, persistent configuration). Re-using a " +
		"key OVERWRITES the existing note. Call note_list first if " +
		"you're not sure whether a key is taken."
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

			if existed {
				return fantasy.ToolResponse{
					Content: fmt.Sprintf("saved (overwrote prior version of %q)", key),
				}, nil
			}
			return fantasy.ToolResponse{
				Content: fmt.Sprintf("saved (new key %q)", key),
			}, nil
		},
	)
}

func noteListTool(store *notes.Store) fantasy.AgentTool {
	desc := "List all stored notes — key, last-updated relative " +
		"time, and a one-line preview of each. Use this as your " +
		"index before deciding whether to call note_show (full body) " +
		"or note_save (with a fresh or re-used key)."
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

func noteShowTool(store *notes.Store) fantasy.AgentTool {
	desc := "Show one note's full content by key. Returns the raw " +
		"body — the previews from note_list are truncated, this is " +
		"the source of truth. Errors with the list of available " +
		"keys if the requested key doesn't exist."
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
			if errors.Is(err, sql.ErrNoRows) {
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

func noteDeleteTool(store *notes.Store) fantasy.AgentTool {
	desc := "Delete a note by key. Idempotent: deleting a key that " +
		"doesn't exist still succeeds (the response distinguishes " +
		"the two cases). Use this to retract facts the user has " +
		"corrected or that no longer apply."
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

// missingKeyHint builds the IsError message for note_show on a
// missing key — same UX pattern as context_show: tell the model
// which keys actually exist so it can pick the right one without
// guessing or hallucinating.
func missingKeyHint(ctx context.Context, store *notes.Store, key string) string {
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
