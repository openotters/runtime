package agent

import (
	"context"
	"fmt"

	"charm.land/fantasy"
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

func (s *Service) ListSessions(ctx context.Context) ([]memory.SessionInfo, error) {
	return s.store.ListSessions(ctx)
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
