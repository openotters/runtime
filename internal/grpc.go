package internal

import (
	"context"

	runtimev1 "github.com/openotters/agentfile/agent/api/v1"
	"github.com/openotters/runtime/pkg/agent"
)

type GRPCServer struct {
	runtimev1.UnimplementedAgentRuntimeServer
	svc       *agent.Service
	agentName string
	model     string
}

func NewGRPCServer(svc *agent.Service, agentName, model string) runtimev1.AgentRuntimeServer {
	return &GRPCServer{svc: svc, agentName: agentName, model: model}
}

func (s *GRPCServer) Chat(ctx context.Context, req *runtimev1.ChatRequest) (*runtimev1.ChatResponse, error) {
	response, err := s.svc.Chat(ctx, req.GetSessionId(), req.GetPrompt())
	if err != nil {
		return nil, err
	}

	return &runtimev1.ChatResponse{Response: response}, nil
}

func (s *GRPCServer) PromptObject(
	ctx context.Context, req *runtimev1.PromptObjectRequest,
) (*runtimev1.PromptObjectResponse, error) {
	object, raw, err := s.svc.PromptObject(
		ctx,
		req.GetPrompt(), req.GetSchemaJson(),
		req.GetSchemaName(), req.GetSchemaDesc(),
	)
	if err != nil {
		return nil, err
	}

	return &runtimev1.PromptObjectResponse{
		ObjectJson: object,
		RawText:    raw,
	}, nil
}

func (s *GRPCServer) ChatStream(
	req *runtimev1.ChatStreamRequest, stream runtimev1.AgentRuntime_ChatStreamServer,
) error {
	cb := func(event agent.StreamEvent) {
		_ = stream.Send(&runtimev1.ChatStreamEvent{
			Type:    event.Type,
			Step:    int32(event.Step), //nolint:gosec // step number is small
			Tool:    event.ToolName,
			Content: event.Content,
		})
	}

	response, err := s.svc.ChatStream(stream.Context(), req.GetSessionId(), req.GetPrompt(), cb)
	if err != nil {
		return err
	}

	return stream.Send(&runtimev1.ChatStreamEvent{
		Type:    "message.create",
		Content: response,
	})
}

func (s *GRPCServer) ListSessions(
	ctx context.Context, _ *runtimev1.ListSessionsRequest,
) (*runtimev1.ListSessionsResponse, error) {
	sessions, err := s.svc.ListSessions(ctx)
	if err != nil {
		return nil, err
	}

	infos := make([]*runtimev1.SessionInfo, len(sessions))
	for i, sess := range sessions {
		infos[i] = &runtimev1.SessionInfo{
			Id:           sess.ID,
			MessageCount: int32(sess.MessageCount), //nolint:gosec // count is small
			LastActive:   sess.LastActive,
		}
	}

	return &runtimev1.ListSessionsResponse{Sessions: infos}, nil
}

func (s *GRPCServer) DeleteSession(
	ctx context.Context, req *runtimev1.DeleteSessionRequest,
) (*runtimev1.DeleteSessionResponse, error) {
	if err := s.svc.DeleteSession(ctx, req.GetSessionId()); err != nil {
		return nil, err
	}

	return &runtimev1.DeleteSessionResponse{}, nil
}

func (s *GRPCServer) ListSessionMessages(
	ctx context.Context, req *runtimev1.ListSessionMessagesRequest,
) (*runtimev1.ListSessionMessagesResponse, error) {
	msgs, err := s.svc.ListSessionMessages(ctx, req.GetSessionId(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}

	out := make([]*runtimev1.SessionMessage, len(msgs))
	for i, m := range msgs {
		out[i] = &runtimev1.SessionMessage{
			Role:      m.Role,
			Content:   m.Content,
			CreatedAt: m.CreatedAt,
		}
	}

	return &runtimev1.ListSessionMessagesResponse{Messages: out}, nil
}

func (s *GRPCServer) Health(
	_ context.Context, _ *runtimev1.HealthRequest,
) (*runtimev1.HealthResponse, error) {
	return &runtimev1.HealthResponse{
		Status:    "ok",
		AgentName: s.agentName,
		Model:     s.model,
	}, nil
}

func (s *GRPCServer) Ready(
	_ context.Context, _ *runtimev1.ReadyRequest,
) (*runtimev1.ReadyResponse, error) {
	return &runtimev1.ReadyResponse{Ready: s.svc != nil}, nil
}
