package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"
	"go.uber.org/zap"

	"github.com/openotters/runtime/pkg/memory"
	"github.com/openotters/runtime/pkg/sessionctx"
)

type Service struct {
	agent     fantasy.Agent
	model     fantasy.LanguageModel
	store     *memory.Store
	compactor *memory.Compactor
	logger    *zap.Logger
}

func NewService(
	agent fantasy.Agent, model fantasy.LanguageModel,
	store *memory.Store, compactor *memory.Compactor, logger *zap.Logger,
) *Service {
	return &Service{
		agent:     agent,
		model:     model,
		store:     store,
		compactor: compactor,
		logger:    logger.Named("agent-service"),
	}
}

func (s *Service) Chat(ctx context.Context, sessionID, prompt string) (string, error) {
	history, err := s.store.GetMessages(ctx, sessionID)
	if err != nil {
		s.logger.Warn("failed to load history", zap.Error(err), zap.String("session", sessionID))
	}

	if err = s.store.SaveMessage(ctx, sessionID, "user", prompt); err != nil {
		s.logger.Warn("failed to save user message", zap.Error(err))
	}

	call := fantasy.AgentCall{
		Prompt:   prompt,
		Messages: history,
	}

	result, err := s.agent.Generate(ctx, call)
	if err != nil {
		return "", fmt.Errorf("agent generate: %w", err)
	}

	response := result.Response.Content.Text()

	if err = s.store.SaveMessage(ctx, sessionID, "assistant", response); err != nil {
		s.logger.Warn("failed to save assistant message", zap.Error(err))
	}

	s.compact(ctx, sessionID)

	s.logger.Info("chat completed",
		zap.String("session", sessionID),
		zap.Int("response_len", len(response)),
	)

	return response, nil
}

type StreamEvent struct {
	Type     string
	Step     int
	ToolName string
	ToolID   string
	Content  string
	// DurationMs is the wall-clock execution time for the event.
	// Populated on tool.result (= OnToolCall→OnToolResult elapsed)
	// and zero on every other type.
	DurationMs int64
}

type StreamCallback func(event StreamEvent)

// StoredPart is the on-disk shape of one piece of an assistant
// response. The dashboard rendering layer mirrors this exactly —
// kind=text accumulates streamed text deltas; kind=tool captures
// one tool call + its result, parsed via the runtime's stream
// callbacks. Stored as an array of StoredPart inside the messages
// row's `content` column for assistant turns.
type StoredPart struct {
	Kind   string `json:"kind"`              // "text" | "tool"
	Text   string `json:"text,omitempty"`    // kind=text
	ToolID string `json:"tool_id,omitempty"` // kind=tool, runtime-issued id
	Name   string `json:"name,omitempty"`    // kind=tool
	Input  string `json:"input,omitempty"`   // kind=tool, raw JSON the model produced
	Output string `json:"output,omitempty"`  // kind=tool
	State  string `json:"state,omitempty"`   // "input-available" | "output-available"
	// DurationMs is the wall-clock time the tool spent executing,
	// measured from OnToolCall to OnToolResult. Populated only on
	// kind=tool parts where the result has come back; zero while
	// state == "input-available". Persisted so historical chat
	// rendering keeps the timing visible after a page refresh.
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// ChatStream runs one streamed turn against the configured model
// and persists the assistant's structured parts. When opts.Regenerate
// is true and a prior assistant turn exists for the session, the
// new parts get appended as a branch to that turn instead of
// inserting a new row, so refresh recovers all alternatives.
//
// Long by design — the streaming pipeline is a single linear
// construction of history load, stub-row insert, per-event
// callbacks (each with its own incremental-persist hook),
// Stream invocation, and final flush. Splitting it across helpers
// would scatter the closure-shared state (parts, assistantRowID,
// flushParts) and make the persistence flow harder to follow.
//
//nolint:funlen // see comment above.
func (s *Service) ChatStream(
	ctx context.Context, sessionID, prompt string, cb StreamCallback, opts ChatStreamOptions,
) (string, error) {
	history, err := s.store.GetMessages(ctx, sessionID)
	if err != nil {
		s.logger.Warn("failed to load history", zap.Error(err), zap.String("session", sessionID))
	}

	// Regenerate doesn't re-save the user prompt — it's still the
	// trailing user turn from the last exchange.
	if !opts.Regenerate {
		if err = s.store.SaveMessage(ctx, sessionID, "user", prompt); err != nil {
			s.logger.Warn("failed to save user message", zap.Error(err))
		}
	}

	// Collected parts for this run. Persisted incrementally via
	// flushParts on every stream event so a runtime crash mid-turn
	// (self_reload, kill, OOM) leaves a recoverable partial assistant
	// row in the daemon's store — the operator's next page refresh
	// shows everything up to the last persisted event, not a ghost
	// row carrying only the user prompt.
	parts := []StoredPart{}
	// toolStarts maps tool_call_id → wall-clock start time so
	// OnToolResult can compute the elapsed duration when the
	// matching result lands. Cleared per-call when the result
	// arrives; never persisted (only the elapsed value is).
	toolStarts := make(map[string]time.Time)
	pushText := func(chunk string) {
		if len(parts) == 0 || parts[len(parts)-1].Kind != "text" {
			parts = append(parts, StoredPart{Kind: "text", Text: chunk})
			return
		}
		parts[len(parts)-1].Text += chunk
	}

	// Insert the assistant row up front so every stream event has a
	// row to update. Regenerate keeps the old row (the existing
	// branch-rotate path handles persistence on completion); inserting
	// a second row here would double-up the turn.
	var assistantRowID int64
	if !opts.Regenerate {
		id, stubErr := s.store.AppendAssistantStub(ctx, sessionID)
		if stubErr != nil {
			s.logger.Warn("failed to insert assistant stub", zap.Error(stubErr))
		} else {
			assistantRowID = id
		}
	}

	// flushParts marshals the current parts and updates the assistant
	// row in place. Detached context: keeps the write going even
	// when the stream's parent ctx is cancelled (the common case for
	// runtime kill / self_reload). Best-effort — a failed flush logs
	// and lets the stream continue; the next event will retry.
	flushParts := func() {
		if assistantRowID == 0 {
			return
		}
		contentJSON, marshalErr := json.Marshal(parts)
		if marshalErr != nil {
			s.logger.Warn("failed to marshal parts for incremental persist", zap.Error(marshalErr))
			return
		}
		persistCtx, cancel := context.WithTimeout(context.Background(), persistTimeout)
		defer cancel()
		if updErr := s.store.UpdateBranches(persistCtx, assistantRowID, string(contentJSON), "[]", 0); updErr != nil {
			s.logger.Warn("incremental persist failed", zap.Error(updErr))
		}
	}

	call := fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: history,
		OnStepStart: func(stepNumber int) error {
			cb(StreamEvent{Type: "step.start", Step: stepNumber})
			return nil
		},
		OnStepFinish: func(step fantasy.StepResult) error {
			cb(StreamEvent{Type: "step.finish", Content: step.Content.Text()})
			flushParts()
			return nil
		},
		OnTextDelta: func(_, text string) error {
			pushText(text)
			cb(StreamEvent{Type: "text.delta", Content: text})
			flushParts()
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			toolStarts[tc.ToolCallID] = time.Now()
			parts = append(parts, StoredPart{
				Kind:   "tool",
				ToolID: tc.ToolCallID,
				Name:   tc.ToolName,
				Input:  tc.Input,
				State:  "input-available",
			})
			cb(StreamEvent{
				Type: "tool.call", ToolName: tc.ToolName,
				ToolID: tc.ToolCallID, Content: tc.Input,
			})
			flushParts()
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			content := ""
			if text, ok := tr.Result.(fantasy.ToolResultOutputContentText); ok {
				content = text.Text
			}
			var durMs int64
			if start, ok := toolStarts[tr.ToolCallID]; ok {
				durMs = time.Since(start).Milliseconds()
				delete(toolStarts, tr.ToolCallID)
			}
			// Attach to the most recent matching tool call.
			for i := len(parts) - 1; i >= 0; i-- {
				if parts[i].Kind == "tool" && parts[i].ToolID == tr.ToolCallID && parts[i].State == "input-available" {
					parts[i].Output = content
					parts[i].State = "output-available"
					parts[i].DurationMs = durMs
					break
				}
			}
			cb(StreamEvent{
				Type: "tool.result", ToolName: tr.ToolName,
				ToolID: tr.ToolCallID, Content: content, DurationMs: durMs,
			})
			flushParts()
			return nil
		},
	}

	// Tool callbacks need to know which session they're running
	// inside — e.g. job_submit auto-stamps io.openotters.session-id
	// onto submitted jobs. Threading via ctx (not env) because
	// sessions are per-call and the runtime hosts many concurrently.
	result, streamErr := s.agent.Stream(sessionctx.With(ctx, sessionID), call)

	// Final flush. For the streaming-from-empty path the stub row
	// is in place and every event already updated it — this is the
	// trailing best-effort sync after Stream's last callback fired.
	// For the regenerate path, hand off to persistAssistantTurn which
	// slides the prior content into branches and overwrites the row.
	//
	// Use a detached context so a cancelled parent ctx (the common
	// reason for the cancel branch) doesn't immediately kill the
	// SQLite write too.
	switch {
	case opts.Regenerate && (streamErr == nil || len(parts) > 0):
		persistCtx, persistCancel := context.WithTimeout(context.Background(), persistTimeout)
		if persistErr := s.persistAssistantTurn(persistCtx, sessionID, parts, true); persistErr != nil {
			s.logger.Warn("failed to persist assistant turn", zap.Error(persistErr))
		}
		persistCancel()
	case assistantRowID != 0:
		// Stub-row path: a final flush captures any state set
		// between the last callback and Stream's return.
		flushParts()
	}

	if streamErr != nil {
		// Compaction is a steady-state housekeeping pass; skip it on
		// interrupted turns so we don't compound the user's cancel
		// with extra latency / a possible second error path.
		return "", fmt.Errorf("agent stream: %w", streamErr)
	}

	s.compact(ctx, sessionID)

	response := result.Response.Content.Text()
	s.logger.Info("chat stream completed",
		zap.String("session", sessionID),
		zap.Int("steps", len(result.Steps)),
		zap.Int("parts", len(parts)),
		zap.Bool("regenerate", opts.Regenerate),
	)

	return response, nil
}

// persistTimeout bounds the detached Save we do after a cancelled
// (or otherwise errored) stream. 2s is comfortably above SQLite's
// per-write latency on local disk and short enough that a hung DB
// doesn't cascade into a perceptible cancel delay.
const persistTimeout = 2 * time.Second

// ChatStreamOptions modulates a ChatStream invocation.
type ChatStreamOptions struct {
	// Regenerate signals the runtime to attach the produced parts
	// as a new branch onto the most recent assistant turn for the
	// session. Falls back to a fresh row if no prior assistant
	// turn exists.
	Regenerate bool
}

// persistAssistantTurn writes the structured parts for one
// assistant response. Inserts a fresh row by default; on
// Regenerate slides the existing content into branches and
// activates the new parts.
func (s *Service) persistAssistantTurn(
	ctx context.Context, sessionID string, parts []StoredPart, regenerate bool,
) error {
	contentJSON, err := json.Marshal(parts)
	if err != nil {
		return fmt.Errorf("encoding parts: %w", err)
	}

	if regenerate {
		prior, lookupErr := s.store.LastAssistantMessage(ctx, sessionID)
		if lookupErr == nil {
			var branches []json.RawMessage
			if prior.BranchesJSON != "" {
				_ = json.Unmarshal([]byte(prior.BranchesJSON), &branches)
			}
			branches = append(branches, json.RawMessage(prior.Content))
			branchesJSON, marshalErr := json.Marshal(branches)
			if marshalErr != nil {
				return fmt.Errorf("encoding branches: %w", marshalErr)
			}
			active := len(branches)
			return s.store.UpdateBranches(ctx, prior.ID, string(contentJSON), string(branchesJSON), active)
		}
	}

	return s.store.SaveMessage(ctx, sessionID, "assistant", string(contentJSON))
}

// PromptObject runs a one-shot, stateless structured-output query
// against the underlying LanguageModel. No session memory is loaded
// or saved, no tool loop is run — just prompt + schema in, parsed
// object out. Matches fantasy.LanguageModel.GenerateObject one-to-one
// and exists so the runtime's gRPC surface has a place to hang the
// call without exposing the model type through Service's public API.
//
// schemaJSON must be a JSON Schema document that unmarshals into
// fantasy/schema.Schema (common subset: type, properties, required,
// items, enum, format, min/max). schemaName and schemaDesc surface
// in tool-mode providers as the synthetic tool's name/description.
//
// ObjectMode (JSON / tool / text / auto) is provider-level — set
// when the LanguageModel is constructed via e.g.
// anthropic.WithObjectMode(...). This method doesn't override it
// per-call because fantasy.ObjectCall has no per-call mode field.
//
// Returns (objectJSON, rawText). rawText is the model's unparsed
// reply — useful for debugging when repair was needed.
func (s *Service) PromptObject(
	ctx context.Context,
	prompt string, schemaJSON []byte, schemaName, schemaDesc string,
) ([]byte, string, error) {
	if s.model == nil {
		return nil, "", fmt.Errorf("no language model bound to service")
	}

	if len(schemaJSON) == 0 {
		return nil, "", fmt.Errorf("schema is required")
	}

	var parsed schema.Schema
	if err := json.Unmarshal(schemaJSON, &parsed); err != nil {
		return nil, "", fmt.Errorf("parsing schema: %w", err)
	}

	if parsed.Type == "" {
		return nil, "", fmt.Errorf("schema must declare a top-level type")
	}

	call := fantasy.ObjectCall{
		Prompt:            fantasy.Prompt{fantasy.NewUserMessage(prompt)},
		Schema:            parsed,
		SchemaName:        schemaName,
		SchemaDescription: schemaDesc,
	}

	resp, err := s.model.GenerateObject(ctx, call)
	if err != nil {
		return nil, "", fmt.Errorf("generate object: %w", err)
	}

	out, err := json.Marshal(resp.Object)
	if err != nil {
		return nil, resp.RawText, fmt.Errorf("marshal object: %w", err)
	}

	s.logger.Info("prompt object completed",
		zap.Int("response_bytes", len(out)),
		zap.Int("raw_bytes", len(resp.RawText)),
	)

	return out, resp.RawText, nil
}

func (s *Service) ListSessions(ctx context.Context) ([]memory.SessionInfo, error) {
	return s.store.ListSessions(ctx)
}

// SessionMessage is the plain wire-ready view of a stored chat message:
// role, text, and its creation time. Compacted or summarised entries
// already flow through the store as role=assistant, so callers get a
// post-compaction view.
type SessionMessage struct {
	Role         string
	Content      string
	BranchesJSON string
	ActiveBranch int32
	CreatedAt    int64
}

// ListSessionMessages returns the recent messages stored for sessionID
// in role/content form suitable for gRPC transport. limit <= 0 means
// "use the store default" (LIMIT 50 today).
func (s *Service) ListSessionMessages(ctx context.Context, sessionID string, _ int) ([]SessionMessage, error) {
	stored, err := s.store.ListMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	out := make([]SessionMessage, 0, len(stored))
	for _, m := range stored {
		if m.Content == "" {
			continue
		}
		out = append(out, SessionMessage{
			Role:         m.Role,
			Content:      m.Content,
			BranchesJSON: m.BranchesJSON,
			ActiveBranch: int32(m.ActiveBranch), //nolint:gosec // small int, daemon-bounded
			CreatedAt:    m.CreatedAt.Unix(),
		})
	}

	return out, nil
}

func (s *Service) DeleteSession(ctx context.Context, sessionID string) error {
	return s.store.DeleteSession(ctx, sessionID)
}

func (s *Service) compact(ctx context.Context, sessionID string) {
	if s.compactor == nil {
		return
	}

	if err := s.compactor.Compact(ctx, s.model, s.store, sessionID); err != nil {
		s.logger.Warn("compaction failed", zap.Error(err), zap.String("session", sessionID))
	}
}
