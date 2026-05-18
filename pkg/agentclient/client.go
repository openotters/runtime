// Package agentclient is the runtime-side client to the daemon's
// agent-to-agent linking RPCs. It exposes the four agent-facing
// calls the LLM tools (agent_list / agent_info / agent_chat /
// agent_exec) need; the daemon enforces "target ∈ JWT.Links" on
// every call.
//
// Configuration parallels pkg/jobsclient — OTTERSD_URL +
// OTTERS_AGENT_TOKEN planted in the spawn env by the executor.
// Both clients share the underlying gRPC channel topology and
// auth shape; the package boundary just keeps each client focused
// on one daemon surface (jobs vs links) for readability.
package agentclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"

	daemonv1 "github.com/openotters/openotters/api/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Env names — same vars jobsclient reads. Re-exported so tests
// don't pull jobsclient in just for the constant.
const (
	EnvDaemonURL = "OTTERSD_URL"
	// EnvAgentToken is the spawn-env var that carries the agent's
	// JWT. The string is an env-var NAME, not a credential — the
	// actual token value lives only in process memory.
	EnvAgentToken = "OTTERS_AGENT_TOKEN" //nolint:gosec // G101: env var name, not a literal credential.
)

// Config holds the dial inputs.
type Config struct {
	URL   string
	Token string
}

// FromEnv reads config from the runtime's process env. Returns
// (zero, false) when either var is missing — the tool builder
// treats this as "no daemon callback path" and registers no
// agent_* tools.
func FromEnv() (Config, bool) {
	u := strings.TrimSpace(os.Getenv(EnvDaemonURL))
	t := strings.TrimSpace(os.Getenv(EnvAgentToken))
	if u == "" || t == "" {
		return Config{}, false
	}
	return Config{URL: u, Token: t}, true
}

// Client wraps daemonv1.RuntimeClient with bearer-token injection
// and lazy dialing. Safe for concurrent use.
type Client struct {
	cfg  Config
	once sync.Once
	conn *grpc.ClientConn
	rt   daemonv1.RuntimeClient
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
		c.err = errors.New("agentclient: OTTERSD_URL and OTTERS_AGENT_TOKEN must both be set")
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
		c.err = fmt.Errorf("agentclient: dial %s: %w", target, err)
		return
	}
	c.conn = conn
	c.rt = daemonv1.NewRuntimeClient(conn)
}

func (c *Client) ensure() error {
	c.once.Do(c.dial)
	return c.err
}

// AgentView is the agent-facing snapshot of one linked agent.
// Mirrors the proto LinkedAgent with JSON tags so the runtime
// can return it directly as the tool response payload.
type AgentView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Model  string `json:"model"`
	Status string `json:"status"`
}

// AgentInfoView is the richer payload for agent_info. Adds
// description (the target's `description` label) + the list of
// capabilities the target exposes so the calling agent can decide
// whether it's the right delegate.
type AgentInfoView struct {
	AgentView
	Description  string   `json:"description,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// AgentList returns every agent the caller is linked to. The
// daemon resolves the caller from the JWT — the request body has
// no agent_id field, so this is unspoofable.
func (c *Client) AgentList(ctx context.Context) ([]AgentView, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}
	resp, err := c.rt.AgentList(ctx, &daemonv1.AgentListRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]AgentView, 0, len(resp.GetAgents()))
	for _, a := range resp.GetAgents() {
		out = append(out, agentFromProto(a))
	}
	return out, nil
}

// AgentInfo returns the target's metadata + capabilities.
// PermissionDenied if the target isn't in the caller's JWT.Links.
func (c *Client) AgentInfo(ctx context.Context, ref string) (AgentInfoView, error) {
	if err := c.ensure(); err != nil {
		return AgentInfoView{}, err
	}
	resp, err := c.rt.AgentInfo(ctx, &daemonv1.AgentInfoRequest{Ref: ref})
	if err != nil {
		return AgentInfoView{}, err
	}
	return AgentInfoView{
		AgentView:    agentFromProto(resp.GetAgent()),
		Description:  resp.GetDescription(),
		Capabilities: resp.GetCapabilities(),
	}, nil
}

// AgentChat sends a prompt to the target and blocks until the
// target's turn finishes. sessionID is optional; pass an empty
// string for a fresh session, or thread the returned id through
// subsequent calls. Returns (response, returnedSessionID, err).
func (c *Client) AgentChat(
	ctx context.Context, ref, prompt, sessionID string,
) (string, string, error) {
	if err := c.ensure(); err != nil {
		return "", "", err
	}
	resp, err := c.rt.AgentChat(ctx, &daemonv1.AgentChatRequest{
		Ref: ref, Prompt: prompt, SessionId: sessionID,
	})
	if err != nil {
		return "", "", err
	}
	return resp.GetResponse(), resp.GetSessionId(), nil
}

// AgentExec is the stateless one-shot variant. No session memory
// on the target.
func (c *Client) AgentExec(ctx context.Context, ref, prompt string) (string, error) {
	if err := c.ensure(); err != nil {
		return "", err
	}
	resp, err := c.rt.AgentExec(ctx, &daemonv1.AgentExecRequest{
		Ref: ref, Prompt: prompt,
	})
	if err != nil {
		return "", err
	}
	return resp.GetResponse(), nil
}

func agentFromProto(a *daemonv1.LinkedAgent) AgentView {
	if a == nil {
		return AgentView{}
	}
	return AgentView{
		ID:     a.GetId(),
		Name:   a.GetName(),
		Model:  a.GetModel(),
		Status: a.GetStatus(),
	}
}

// dialTarget mirrors jobsclient.dialTarget. Duplicated rather
// than shared so neither package becomes the canonical owner of
// the runtime ↔ daemon URL grammar — both consume the same env
// var directly.
func dialTarget(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("agentclient: parse OTTERSD_URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" && u.Opaque != "" {
			path = u.Opaque
		}
		if path == "" {
			return "", fmt.Errorf("agentclient: OTTERSD_URL %q has no socket path", raw)
		}
		return "unix:" + path, nil
	case "http":
		host := u.Host
		if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
			host = net.JoinHostPort(host, "80")
		}
		return host, nil
	default:
		return "", fmt.Errorf("agentclient: OTTERSD_URL %q: unsupported scheme %q (use unix:// or http://)",
			raw, u.Scheme)
	}
}

func bearerUnary(token string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
	) error {
		ctx = withBearer(ctx, token)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func bearerStream(token string) grpc.StreamClientInterceptor {
	return func(
		ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
		method string, streamer grpc.Streamer, opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		ctx = withBearer(ctx, token)
		return streamer(ctx, desc, cc, method, opts...)
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
