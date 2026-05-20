// Package memoryclient is the runtime-side client to the daemon's
// agent_messages table. Mirrors the legacy pkg/memory.Store API
// surface so pkg/agent/service.go's chat path and pkg/memory's
// compactor can swap stores for the daemon client without changing
// call sites.
//
// Like notesclient, the daemon scopes every call to the JWT's
// agent_ref, so empty ref on the wire makes the daemon read the
// agent identity from the bearer token.
package memoryclient

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	daemonv1 "github.com/openotters/openotters/api/v1"
)

const (
	EnvDaemonURL = "OTTERSD_URL"
	//nolint:gosec // G101: env var name, not a credential value
	EnvAgentToken = "OTTERS_AGENT_TOKEN"
)

// ErrNotConfigured is returned when both env vars aren't set.
var ErrNotConfigured = errors.New(
	"memoryclient: OTTERSD_URL and OTTERS_AGENT_TOKEN must both be set",
)

type Config struct {
	URL   string
	Token string
}

func FromEnv() (Config, bool) {
	u := strings.TrimSpace(os.Getenv(EnvDaemonURL))
	tok := strings.TrimSpace(os.Getenv(EnvAgentToken))
	if u == "" || tok == "" {
		return Config{}, false
	}
	return Config{URL: u, Token: tok}, true
}

type Client struct {
	cfg  Config
	once sync.Once
	conn *grpc.ClientConn
	rt   daemonv1.AgentStateClient
	err  error
}

func New(cfg Config) *Client { return &Client{cfg: cfg} }

func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *Client) dial() {
	if c.cfg.URL == "" || c.cfg.Token == "" {
		c.err = ErrNotConfigured
		return
	}
	target, err := dialTarget(c.cfg.URL)
	if err != nil {
		c.err = err
		return
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(bearerUnary(c.cfg.Token)),
		grpc.WithStreamInterceptor(bearerStream(c.cfg.Token)),
	)
	if err != nil {
		c.err = fmt.Errorf("memoryclient: dial %s: %w", target, err)
		return
	}
	c.conn = conn
	c.rt = daemonv1.NewAgentStateClient(conn)
}

func (c *Client) ensure() error {
	c.once.Do(c.dial)
	return c.err
}

// StoredMessage mirrors pkg/memory.StoredMessage — the timestamp-
// bearing shape returned by ListMessages.
type StoredMessage struct {
	ID           int64
	Role         string
	Content      string
	BranchesJSON string
	ActiveBranch int
	CreatedAt    time.Time
}

// SessionInfo mirrors pkg/memory.SessionInfo.
type SessionInfo struct {
	ID           string
	MessageCount int
	LastActive   time.Time
}

// GetMessages returns the LLM-facing history for sessionID,
// expanded into fantasy.Message form. Assistant rows carrying
// JSON parts are flattened into the (assistant, tool) sequence
// providers expect — same conversion as the legacy memory.Store.
func (c *Client) GetMessages(ctx context.Context, sessionID string) ([]fantasy.Message, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}
	resp, err := c.rt.ListMessages(ctx, &daemonv1.StateListMessagesRequest{
		SessionId: sessionID,
	})
	if err != nil {
		return nil, err
	}
	var out []fantasy.Message
	for _, m := range resp.GetMessages() {
		if m.GetRole() == "assistant" {
			out = append(out, expandAssistantParts(m.GetContent())...)
			continue
		}
		out = append(out, fantasy.Message{
			Role:    fantasy.MessageRole(m.GetRole()),
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: m.GetContent()}},
		})
	}
	return out, nil
}

// ListMessages returns the timestamped raw rows for sessionID,
// suitable for UI/diagnostic rendering. No part expansion.
func (c *Client) ListMessages(ctx context.Context, sessionID string) ([]StoredMessage, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}
	resp, err := c.rt.ListMessages(ctx, &daemonv1.StateListMessagesRequest{
		SessionId: sessionID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]StoredMessage, 0, len(resp.GetMessages()))
	for _, m := range resp.GetMessages() {
		out = append(out, StoredMessage{
			ID:           m.GetId(),
			Role:         m.GetRole(),
			Content:      m.GetContent(),
			BranchesJSON: m.GetBranchesJson(),
			ActiveBranch: int(m.GetActiveBranch()),
			CreatedAt:    time.Unix(m.GetCreatedUnix(), 0),
		})
	}
	return out, nil
}

// SaveMessage appends one message row at the tail. Mirrors
// memory.Store.SaveMessage. The returned id is the daemon's
// primary key; callers that don't need it (most do not) can
// discard it.
func (c *Client) SaveMessage(ctx context.Context, sessionID, role, content string) (int64, error) {
	if err := c.ensure(); err != nil {
		return 0, err
	}
	resp, err := c.rt.AppendMessage(ctx, &daemonv1.StateAppendMessageRequest{
		SessionId: sessionID,
		Role:      role,
		Content:   content,
	})
	if err != nil {
		return 0, err
	}
	return resp.GetId(), nil
}

// AppendAssistantStub inserts a placeholder assistant row with
// content = "[]" and returns its id. The streaming chat loop
// holds onto the id and updates the row via UpdateBranches as
// deltas arrive.
func (c *Client) AppendAssistantStub(ctx context.Context, sessionID string) (int64, error) {
	return c.SaveMessage(ctx, sessionID, "assistant", "[]")
}

// UpdateBranches rewrites one assistant row's content + branches
// + active. Mirrors memory.Store.UpdateBranches.
func (c *Client) UpdateBranches(
	ctx context.Context, id int64, content, branchesJSON string, active int,
) error {
	if err := c.ensure(); err != nil {
		return err
	}
	_, err := c.rt.UpdateMessageBranches(ctx, &daemonv1.StateUpdateBranchesRequest{
		Id:           id,
		Content:      content,
		BranchesJson: branchesJSON,
		ActiveBranch: int32(active), //nolint:gosec // caller-bounded
	})
	return err
}

// LastAssistantMessage returns the most recent assistant row for
// sessionID, or sql.ErrNoRows if none exists. The sentinel
// matches memory.Store's contract so callers can errors.Is
// against the same value.
func (c *Client) LastAssistantMessage(ctx context.Context, sessionID string) (StoredMessage, error) {
	if err := c.ensure(); err != nil {
		return StoredMessage{}, err
	}
	resp, err := c.rt.LastAssistantMessage(ctx, &daemonv1.StateLastAssistantRequest{
		SessionId: sessionID,
	})
	if err != nil {
		return StoredMessage{}, err
	}
	if !resp.GetFound() {
		return StoredMessage{}, sql.ErrNoRows
	}
	m := resp.GetMessage()
	return StoredMessage{
		ID:           m.GetId(),
		Role:         m.GetRole(),
		Content:      m.GetContent(),
		BranchesJSON: m.GetBranchesJson(),
		ActiveBranch: int(m.GetActiveBranch()),
		CreatedAt:    time.Unix(m.GetCreatedUnix(), 0),
	}, nil
}

// ReplaceMessages atomically replaces every message for sessionID
// with the supplied list. Used by the compactor after slide/
// summarize collapses the in-memory history.
//
// Each message's Content is plain text for user/system rows and
// JSON-encoded parts for assistant rows; this client passes them
// through verbatim — the runtime decides the shape.
func (c *Client) ReplaceMessages(
	ctx context.Context, sessionID string, msgs []StoredMessage,
) error {
	if err := c.ensure(); err != nil {
		return err
	}
	rows := make([]*daemonv1.MessageRow, 0, len(msgs))
	for _, m := range msgs {
		rows = append(rows, &daemonv1.MessageRow{
			Role:         m.Role,
			Content:      m.Content,
			BranchesJson: m.BranchesJSON,
			ActiveBranch: int32(m.ActiveBranch), //nolint:gosec // caller-bounded
		})
	}
	_, err := c.rt.ReplaceMessages(ctx, &daemonv1.StateReplaceMessagesRequest{
		SessionId: sessionID,
		Messages:  rows,
	})
	return err
}

// ListSessions enumerates the agent's sessions.
func (c *Client) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}
	resp, err := c.rt.ListSessions(ctx, &daemonv1.StateListSessionsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(resp.GetSessions()))
	for _, s := range resp.GetSessions() {
		out = append(out, SessionInfo{
			ID:           s.GetId(),
			MessageCount: int(s.GetMessageCount()),
			LastActive:   time.Unix(s.GetLastActive(), 0),
		})
	}
	return out, nil
}

// DeleteSession removes one session's messages. Idempotent.
func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	if err := c.ensure(); err != nil {
		return err
	}
	_, err := c.rt.DeleteSession(ctx, &daemonv1.StateDeleteSessionRequest{
		SessionId: sessionID,
	})
	return err
}

// storedAssistantPart mirrors pkg/agent.StoredPart and
// pkg/memory.storedAssistantPart so we can decode assistant-row
// JSON without depending on either of those packages.
type storedAssistantPart struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	ToolID string `json:"tool_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
	Output string `json:"output"`
	State  string `json:"state"`
}

// expandAssistantParts is a verbatim copy of pkg/memory.expandAssistantParts
// (kept in sync if that ever changes). Splits one assistant-row's
// parts JSON into the (assistant, tool) message sequence the
// provider expects on replay.
func expandAssistantParts(content string) []fantasy.Message {
	var parts []storedAssistantPart
	if err := json.Unmarshal([]byte(content), &parts); err != nil || len(parts) == 0 {
		return []fantasy.Message{{
			Role:    fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: content}},
		}}
	}

	var out []fantasy.Message
	assistant := fantasy.Message{Role: fantasy.MessageRoleAssistant}
	tool := fantasy.Message{Role: fantasy.MessageRoleTool}
	sawToolInStep := false

	flushStep := func() {
		if len(assistant.Content) > 0 {
			out = append(out, assistant)
		}
		if len(tool.Content) > 0 {
			out = append(out, tool)
		}
		assistant = fantasy.Message{Role: fantasy.MessageRoleAssistant}
		tool = fantasy.Message{Role: fantasy.MessageRoleTool}
		sawToolInStep = false
	}

	for _, p := range parts {
		switch p.Kind {
		case "text":
			if p.Text == "" {
				continue
			}
			if sawToolInStep {
				flushStep()
			}
			assistant.Content = append(assistant.Content, fantasy.TextPart{Text: p.Text})
		case "tool":
			assistant.Content = append(assistant.Content, fantasy.ToolCallPart{
				ToolCallID: p.ToolID,
				ToolName:   p.Name,
				Input:      p.Input,
			})
			outputText := p.Output
			if p.State != "output-available" || outputText == "" {
				outputText = "(tool call was interrupted before a result was captured)"
			}
			tool.Content = append(tool.Content, fantasy.ToolResultPart{
				ToolCallID: p.ToolID,
				Output:     fantasy.ToolResultOutputContentText{Text: outputText},
			})
			sawToolInStep = true
		}
	}
	flushStep()
	return out
}

func dialTarget(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("memoryclient: parse OTTERSD_URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" && u.Opaque != "" {
			path = u.Opaque
		}
		if path == "" {
			return "", fmt.Errorf("memoryclient: OTTERSD_URL %q has no socket path", raw)
		}
		return "unix:" + path, nil
	case "http":
		host := u.Host
		if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
			host = net.JoinHostPort(host, "80")
		}
		return host, nil
	default:
		return "", fmt.Errorf("memoryclient: OTTERSD_URL %q: unsupported scheme %q", raw, u.Scheme)
	}
}

func bearerUnary(token string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
	) error {
		return invoker(withBearer(ctx, token), method, req, reply, cc, opts...)
	}
}

func bearerStream(token string) grpc.StreamClientInterceptor {
	return func(
		ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
		method string, streamer grpc.Streamer, opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		return streamer(withBearer(ctx, token), desc, cc, method, opts...)
	}
}

func withBearer(ctx context.Context, token string) context.Context {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if existing := md.Get("authorization"); len(existing) > 0 {
			return ctx
		}
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}
