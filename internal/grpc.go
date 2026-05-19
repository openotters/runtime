package internal

import (
	"context"
	"database/sql"
	"errors"

	runtimev1 "github.com/openotters/agentfile/executor/api/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/openotters/runtime/pkg/agent"
	"github.com/openotters/runtime/pkg/notes"
)

type GRPCServer struct {
	runtimev1.UnimplementedAgentRuntimeServer
	svc          *agent.Service
	notesStore   *notes.Store
	notesMaxByte int
	notesMaxCnt  int
	agentName    string
	model        string
}

// NewGRPCServer constructs the runtime's gRPC server. notesStore /
// maxBytes / maxCount are optional: when nil/0 the Notes RPCs reply
// with codes.Unavailable so a dev invocation of the runtime binary
// without a notes store doesn't silently accept writes.
func NewGRPCServer(
	svc *agent.Service,
	notesStore *notes.Store, notesMaxBytes, notesMaxCount int,
	agentName, model string,
) runtimev1.AgentRuntimeServer {
	return &GRPCServer{
		svc:          svc,
		notesStore:   notesStore,
		notesMaxByte: notesMaxBytes,
		notesMaxCnt:  notesMaxCount,
		agentName:    agentName,
		model:        model,
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
			Role:         m.Role,
			Content:      m.Content,
			CreatedAt:    m.CreatedAt,
			BranchesJson: m.BranchesJSON,
			ActiveBranch: m.ActiveBranch,
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

// Notes RPCs — back the per-agent notes store with the same
// pkg/notes Store the tool layer uses. The daemon proxies these to
// the operator UI; the model still goes through note_* tools.

func (s *GRPCServer) ListNotes(
	ctx context.Context, req *runtimev1.ListNotesRequest,
) (*runtimev1.ListNotesResponse, error) {
	if s.notesStore == nil {
		return nil, status.Error(codes.Unavailable, "notes store not configured")
	}
	var (
		all []notes.Note
		err error
	)
	if req.GetOnlyInContext() {
		all, err = s.notesStore.ListInContext(ctx)
	} else {
		all, err = s.notesStore.List(ctx)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list notes: %s", err)
	}
	out := make([]*runtimev1.Note, 0, len(all))
	for _, n := range all {
		// List responses omit Content to keep payloads small —
		// clients use GetNote when they need the body.
		out = append(out, noteToProto(n, false))
	}
	return &runtimev1.ListNotesResponse{Notes: out}, nil
}

func (s *GRPCServer) GetNote(
	ctx context.Context, req *runtimev1.GetNoteRequest,
) (*runtimev1.GetNoteResponse, error) {
	if s.notesStore == nil {
		return nil, status.Error(codes.Unavailable, "notes store not configured")
	}
	n, err := s.notesStore.Get(ctx, req.GetKey())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Errorf(codes.NotFound, "no note %q", req.GetKey())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get note: %s", err)
	}
	return &runtimev1.GetNoteResponse{Note: noteToProto(n, true)}, nil
}

func (s *GRPCServer) SaveNote(
	ctx context.Context, req *runtimev1.SaveNoteRequest,
) (*runtimev1.SaveNoteResponse, error) {
	if s.notesStore == nil {
		return nil, status.Error(codes.Unavailable, "notes store not configured")
	}

	maxBytes := int(req.GetMaxBytes())
	if maxBytes == 0 {
		maxBytes = s.notesMaxByte
	}
	maxCount := int(req.GetMaxCount())
	if maxCount == 0 {
		maxCount = s.notesMaxCnt
	}

	_, prevErr := s.notesStore.Get(ctx, req.GetKey())
	overwrote := prevErr == nil

	if err := s.notesStore.Save(ctx, req.GetKey(), req.GetContent(), maxBytes, maxCount); err != nil {
		switch {
		case errors.Is(err, notes.ErrInvalidKey):
			return nil, status.Errorf(codes.InvalidArgument, "%s", err)
		case errors.Is(err, notes.ErrNoteTooLarge), errors.Is(err, notes.ErrTooManyNotes):
			return nil, status.Errorf(codes.FailedPrecondition, "%s", err)
		default:
			return nil, status.Errorf(codes.Internal, "save note: %s", err)
		}
	}

	n, err := s.notesStore.Get(ctx, req.GetKey())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "re-read after save: %s", err)
	}
	return &runtimev1.SaveNoteResponse{Note: noteToProto(n, true), Overwrote: overwrote}, nil
}

func (s *GRPCServer) DeleteNote(
	ctx context.Context, req *runtimev1.DeleteNoteRequest,
) (*runtimev1.DeleteNoteResponse, error) {
	if s.notesStore == nil {
		return nil, status.Error(codes.Unavailable, "notes store not configured")
	}
	_, getErr := s.notesStore.Get(ctx, req.GetKey())
	existed := getErr == nil
	if err := s.notesStore.Delete(ctx, req.GetKey()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete note: %s", err)
	}
	return &runtimev1.DeleteNoteResponse{Deleted: existed}, nil
}

func (s *GRPCServer) SetNoteInContext(
	ctx context.Context, req *runtimev1.SetNoteInContextRequest,
) (*runtimev1.SetNoteInContextResponse, error) {
	if s.notesStore == nil {
		return nil, status.Error(codes.Unavailable, "notes store not configured")
	}
	err := s.notesStore.SetInContext(ctx, req.GetKey(), req.GetInContext())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Errorf(codes.NotFound, "no note %q", req.GetKey())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set in_context: %s", err)
	}
	n, err := s.notesStore.Get(ctx, req.GetKey())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "re-read after pin: %s", err)
	}
	return &runtimev1.SetNoteInContextResponse{Note: noteToProto(n, true)}, nil
}

// noteToProto converts a pkg/notes Note into its wire shape. When
// includeContent is false the body is dropped — keeps list-response
// payloads bounded since notes can be a few KB each.
func noteToProto(n notes.Note, includeContent bool) *runtimev1.Note {
	pn := &runtimev1.Note{
		Key:       n.Key,
		Preview:   n.Preview,
		InContext: n.InContext,
		CreatedAt: n.CreatedAt.Unix(),
		UpdatedAt: n.UpdatedAt.Unix(),
	}
	if includeContent {
		pn.Content = n.Content
	}
	return pn
}
