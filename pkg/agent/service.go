package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"
	"go.uber.org/zap"

	"github.com/openotters/runtime/pkg/memory"
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
}

type StreamCallback func(event StreamEvent)

func (s *Service) ChatStream(
	ctx context.Context, sessionID, prompt string, cb StreamCallback,
) (string, error) {
	history, err := s.store.GetMessages(ctx, sessionID)
	if err != nil {
		s.logger.Warn("failed to load history", zap.Error(err), zap.String("session", sessionID))
	}

	if err = s.store.SaveMessage(ctx, sessionID, "user", prompt); err != nil {
		s.logger.Warn("failed to save user message", zap.Error(err))
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
			return nil
		},
		OnTextDelta: func(_, text string) error {
			cb(StreamEvent{Type: "text.delta", Content: text})
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			cb(StreamEvent{
				Type: "tool.call", ToolName: tc.ToolName,
				ToolID: tc.ToolCallID, Content: tc.Input,
			})
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			content := ""
			if text, ok := tr.Result.(fantasy.ToolResultOutputContentText); ok {
				content = text.Text
			}

			cb(StreamEvent{
				Type: "tool.result", ToolName: tr.ToolName,
				ToolID: tr.ToolCallID, Content: content,
			})

			return nil
		},
	}

	result, err := s.agent.Stream(ctx, call)
	if err != nil {
		return "", fmt.Errorf("agent stream: %w", err)
	}

	response := result.Response.Content.Text()

	if err = s.store.SaveMessage(ctx, sessionID, "assistant", response); err != nil {
		s.logger.Warn("failed to save assistant message", zap.Error(err))
	}

	s.compact(ctx, sessionID)

	s.logger.Info("chat stream completed",
		zap.String("session", sessionID),
		zap.Int("steps", len(result.Steps)),
		zap.Int("response_len", len(response)),
	)

	return response, nil
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
	Role      string
	Content   string
	CreatedAt int64
}

// ListSessionMessages returns the recent messages stored for sessionID
// in role/content form suitable for gRPC transport. limit <= 0 means
// "use the store default" (LIMIT 50 today).
func (s *Service) ListSessionMessages(ctx context.Context, sessionID string, _ int) ([]SessionMessage, error) {
	fantasyMessages, err := s.store.GetMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	out := make([]SessionMessage, 0, len(fantasyMessages))

	for _, m := range fantasyMessages {
		text := ""
		for _, part := range m.Content {
			if tp, ok := part.(fantasy.TextPart); ok {
				text = tp.Text

				break
			}
		}

		if text == "" {
			continue
		}

		out = append(out, SessionMessage{
			Role:    string(m.Role),
			Content: text,
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
