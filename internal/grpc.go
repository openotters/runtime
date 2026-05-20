package internal

import (
	"context"

	runtimev1 "github.com/openotters/agentfile/executor/api/v1"

	"github.com/openotters/runtime/pkg/agent"
)

type GRPCServer struct {
	runtimev1.UnimplementedAgentRuntimeServer
	svc       *agent.Service
	agentName string
	model     string
}

func NewGRPCServer(svc *agent.Service, agentName, model string) runtimev1.AgentRuntimeServer {
	return &GRPCServer{
		svc:       svc,
		agentName: agentName,
		model:     model,
	}
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
			Type:       event.Type,
			Step:       int32(event.Step), //nolint:gosec // step number is small
			Tool:       event.ToolName,
			Content:    event.Content,
			ToolId:     event.ToolID,
			DurationMs: event.DurationMs,
		})
	}

	response, err := s.svc.ChatStream(
		stream.Context(),
		req.GetSessionId(),
		req.GetPrompt(),
		cb,
		agent.ChatStreamOptions{Regenerate: req.GetRegenerate()},
	)
	if err != nil {
		return err
	}

	return stream.Send(&runtimev1.ChatStreamEvent{
		Type:    "message.create",
		Content: response,
	})
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

// Ready is the daemon supervisor's readiness probe. The daemon hits
// this in a backoff loop after spawning the runtime subprocess; the
// first successful response flips the agent from Starting → Ready.
//
// Two layers of "ready" stack here:
//   - Reachability: until the gRPC listener has bound and accepted
//     the dial, the daemon's probe gets Unavailable / connection-
//     refused and retries. So just answering this RPC at all already
//     means "process up, gRPC alive, accepting traffic."
//   - Service: svc != nil means the agent.Service was constructed —
//     i.e. the model client is loaded, context files are read, the
//     session store is open. The current runtime startup
//     (serve.go) guarantees this before NewGRPCServer is called, so
//     in practice this is always true once the server is up.
//
// Returning {Ready: false} is reserved for a future world where the
// server starts before svc finishes loading (e.g. lazy model init).
// We keep the field so the daemon can keep probing without a wire
// change when that lands.
func (s *GRPCServer) Ready(
	_ context.Context, _ *runtimev1.ReadyRequest,
) (*runtimev1.ReadyResponse, error) {
	return &runtimev1.ReadyResponse{Ready: s.svc != nil}, nil
}
