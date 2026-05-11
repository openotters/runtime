// Package jobsclient is the runtime-side client to the openotters
// daemon's async-jobs API. It exists so an agent's runtime can submit
// long-running BIN jobs against its own spawn env, check on them
// later, or block until they complete — without pinning the agent's
// turn-loop goroutine for the duration.
//
// Configuration comes from two env vars planted in the spawn env by
// the executor (see agentfile/executor/env.go's BuildLockedEnv):
//
//   - OTTERSD_URL          where to dial the daemon. unix://<path>
//                          for the system executor; an http:// URL
//                          for docker (the executor sets
//                          host.docker.internal so the same value
//                          resolves on macOS Docker Desktop and
//                          Linux Docker via ExtraHosts).
//   - OTTERS_AGENT_TOKEN   the JWT minted by the daemon at
//                          CreateAgent. Attached as
//                          `Authorization: Bearer …` on every RPC.
//                          Empty token → constructor returns nil
//                          (caller treats that as "the daemon
//                          callback path isn't wired").
//
// The client lazy-dials on first use so a runtime that never invokes
// a job tool pays nothing for the dependency.
package jobsclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	daemonv1 "github.com/openotters/openotters/api/v1"
)

// Env names — exported so tests / callers can override programmatically
// without re-coding the convention.
const (
	EnvDaemonURL  = "OTTERSD_URL"
	EnvAgentToken = "OTTERS_AGENT_TOKEN"
)

// Config holds the dial inputs. Use FromEnv to populate from the
// spawn env; the explicit struct exists for testing.
type Config struct {
	URL   string // OTTERSD_URL — unix://<path> or http://host:port
	Token string // OTTERS_AGENT_TOKEN — JWT presented as Bearer
}

// FromEnv reads config from the runtime's process environment. Returns
// (zero, false) when either var is missing — the caller treats this
// as "no daemon callback path configured" and skips registering the
// job tools.
func FromEnv() (Config, bool) {
	url := strings.TrimSpace(os.Getenv(EnvDaemonURL))
	tok := strings.TrimSpace(os.Getenv(EnvAgentToken))
	if url == "" || tok == "" {
		return Config{}, false
	}
	return Config{URL: url, Token: tok}, true
}

// Client wraps the daemon's RuntimeClient with bearer-token injection
// and lazy dialing. Safe for concurrent use; the underlying gRPC
// connection multiplexes RPCs.
type Client struct {
	cfg  Config
	once sync.Once
	conn *grpc.ClientConn
	rt   daemonv1.RuntimeClient
	err  error
}

// New constructs a client without dialing. cfg.URL + cfg.Token both
// required — empty values yield an immediate error on first call.
func New(cfg Config) *Client { return &Client{cfg: cfg} }

// Close releases the underlying gRPC connection. Safe to call
// multiple times; first call wins.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// dial is invoked once per Client (sync.Once). Picks the gRPC dial
// target out of the URL — `unix://<path>` becomes `unix:<path>`
// (gRPC's scheme), `http://host:port` becomes `host:port`. Any other
// scheme is rejected up-front so misconfigurations surface early
// rather than as cryptic dial failures later.
func (c *Client) dial() {
	if c.cfg.URL == "" || c.cfg.Token == "" {
		c.err = errors.New("jobsclient: OTTERSD_URL and OTTERS_AGENT_TOKEN must both be set")
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
		c.err = fmt.Errorf("jobsclient: dial %s: %w", target, err)
		return
	}
	c.conn = conn
	c.rt = daemonv1.NewRuntimeClient(conn)
}

func (c *Client) ensure() error {
	c.once.Do(c.dial)
	return c.err
}

// JobView is the agent-facing snapshot of an async job. Mirrors the
// proto's AsyncJob but trims fields the agent doesn't need (handle,
// labels, started_at, …) so the tool's JSON Schema stays small in
// the model's prompt.
type JobView struct {
	JobID    string `json:"job_id"`
	Status   string `json:"status"` // pending|running|done|error|cancelled|orphaned
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

// IsTerminal reports whether the job has reached a final status. The
// daemon's status string is the source of truth; this helper just
// names the four values so call sites read better.
func (v *JobView) IsTerminal() bool {
	switch v.Status {
	case "done", "error", "cancelled", "orphaned":
		return true
	}
	return false
}

// SubmitJob dispatches a BIN against the agent's spawn env. The
// daemon binds agent_ref to the JWT's claim, so the request-side
// value is ignored — agents can't submit jobs on behalf of another
// agent regardless of what they put in the request.
func (c *Client) SubmitJob(
	ctx context.Context, bin string, args []string, stdin string, labels map[string]string,
) (string, error) {
	if err := c.ensure(); err != nil {
		return "", err
	}
	resp, err := c.rt.SubmitAsyncJob(ctx, &daemonv1.SubmitAsyncJobRequest{
		// agent_ref left empty — daemon forces it from the token.
		Bin:    bin,
		Args:   args,
		Stdin:  stdin,
		Labels: labels,
	})
	if err != nil {
		return "", err
	}
	return resp.GetJobId(), nil
}

// GetJob is the non-blocking snapshot. Useful for "did it finish yet"
// polling without holding the agent's turn open.
func (c *Client) GetJob(ctx context.Context, jobID string) (*JobView, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}
	resp, err := c.rt.GetAsyncJob(ctx, &daemonv1.GetAsyncJobRequest{JobId: jobID})
	if err != nil {
		return nil, err
	}
	return jobFromProto(resp.GetJob()), nil
}

// WatchJob blocks until the job reaches a terminal status, then
// returns the final snapshot. Forwards ctx cancellation to a
// CancelAsyncJob call so an interrupted agent turn doesn't leave
// orphaned jobs running on the daemon.
//
// The daemon's WatchAsyncJob is a server-stream that emits the
// current state immediately and on every material change; this
// helper drains until terminal.
func (c *Client) WatchJob(ctx context.Context, jobID string) (*JobView, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}

	// Per-call ctx that we can cancel independently of the parent
	// when forwarding cancellation. Without this, once parent ctx is
	// cancelled the stream Recv would surface ctx.Err and we'd lose
	// the chance to call CancelAsyncJob (the daemon would still
	// reap it via shutdown drain, but the orphan window is wider).
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	stream, err := c.rt.WatchAsyncJob(streamCtx, &daemonv1.WatchAsyncJobRequest{JobId: jobID})
	if err != nil {
		return nil, err
	}

	// Forward parent-ctx cancellation to a CancelAsyncJob call.
	// Bounded with its own short ctx so cancellation doesn't itself
	// hang on a flaky daemon.
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		select {
		case <-ctx.Done():
			cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer ccancel()
			_, _ = c.rt.CancelAsyncJob(cctx, &daemonv1.CancelAsyncJobRequest{JobId: jobID})
			streamCancel()
		case <-streamCtx.Done():
			// Stream finished on its own — nothing to forward.
		}
	}()

	var last *JobView
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Drain the cancel goroutine before returning.
			streamCancel()
			<-cancelDone
			return last, err
		}
		last = jobFromProto(resp.GetJob())
		if last.IsTerminal() {
			break
		}
	}

	streamCancel()
	<-cancelDone

	if last == nil {
		return nil, fmt.Errorf("jobsclient: watch %s: stream closed before any state", jobID)
	}
	return last, nil
}

// FollowJob is the read-only counterpart of WatchJob: it streams
// the job's state until terminal but does NOT cancel the underlying
// job when the caller's context is cancelled. Use it when the caller
// is an *observer* of work owned by someone else — `job_watch` from
// the agent's tool set, for instance, where an interrupted agent
// turn shouldn't yank the BIN it was monitoring.
//
// On ctx-cancel the stream Recv returns with ctx.Err and we return
// (lastSeen, err) — the caller gets whatever snapshot landed
// before the disconnect. The daemon keeps running the job.
//
// Implementation parity with WatchJob is intentional: only the
// cancel-on-disconnect goroutine is omitted. If WatchJob's stream
// loop evolves, this method should track it (extract a shared
// recvLoop helper if it grows).
func (c *Client) FollowJob(ctx context.Context, jobID string) (*JobView, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}

	stream, err := c.rt.WatchAsyncJob(ctx, &daemonv1.WatchAsyncJobRequest{JobId: jobID})
	if err != nil {
		return nil, err
	}

	var last *JobView
	for {
		resp, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			// Parent ctx cancellation lands here as ctx.Err. We
			// intentionally do NOT call CancelAsyncJob — that's
			// the load-bearing difference vs. WatchJob — and
			// return whatever snapshot we already saw alongside
			// the error so observability callers can render
			// partial state.
			return last, recvErr
		}

		last = jobFromProto(resp.GetJob())
		if last.IsTerminal() {
			break
		}
	}

	if last == nil {
		return nil, fmt.Errorf("jobsclient: follow %s: stream closed before any state", jobID)
	}

	return last, nil
}

// ListJobsOpts narrows the agent-side list query. AgentID is NOT
// part of the options: the daemon pins the result set to the JWT's
// bound agent regardless of what the request carries, so the
// runtime never has to know or guess its own UUID. Labels filter
// is AND-merged: every key/value must match.
type ListJobsOpts struct {
	Status string
	Labels map[string]string
}

// ListJobs returns the agent's async-job snapshots, filtered by
// status (optional) and label selector (optional). Used by the
// agent's `job_list` tool to surface "my recent jobs in this
// session" without the agent having to thread its own UUID through
// the call. Order is daemon-side (created_at DESC); the caller
// trims to the desired window.
func (c *Client) ListJobs(ctx context.Context, opts ListJobsOpts) ([]*JobView, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}

	resp, err := c.rt.ListAsyncJobs(ctx, &daemonv1.ListAsyncJobsRequest{
		// agent_id left empty — the daemon scopes by the JWT's
		// bound agent and ignores the request field when the token
		// is agent-scoped (which it always is for the runtime).
		Status:        opts.Status,
		LabelSelector: opts.Labels,
	})
	if err != nil {
		return nil, err
	}

	jobs := resp.GetJobs()
	out := make([]*JobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobFromProto(j))
	}

	return out, nil
}

// CancelJob requests immediate cancellation. The job transitions to
// `cancelled` once the daemon's pool reaps it (~250 ms typical).
func (c *Client) CancelJob(ctx context.Context, jobID string) error {
	if err := c.ensure(); err != nil {
		return err
	}
	_, err := c.rt.CancelAsyncJob(ctx, &daemonv1.CancelAsyncJobRequest{JobId: jobID})
	return err
}

// dialTarget translates a runtime-friendly URL (unix:///path,
// http://host:port) into a gRPC dial target. unix sockets need the
// `unix:<path>` scheme (single slash); http URLs decay to host:port.
func dialTarget(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("jobsclient: parse OTTERSD_URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "unix":
		// url.Parse turns unix:///path into Path=/path; gRPC wants
		// "unix:" + path with a single leading slash.
		path := u.Path
		if path == "" && u.Opaque != "" {
			path = u.Opaque
		}
		if path == "" {
			return "", fmt.Errorf("jobsclient: OTTERSD_URL %q has no socket path", raw)
		}
		return "unix:" + path, nil
	case "http":
		host := u.Host
		if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
			host = net.JoinHostPort(host, "80")
		}
		return host, nil
	default:
		return "", fmt.Errorf("jobsclient: OTTERSD_URL %q: unsupported scheme %q (use unix:// or http://)",
			raw, u.Scheme)
	}
}

func jobFromProto(j *daemonv1.AsyncJob) *JobView {
	if j == nil {
		return nil
	}
	return &JobView{
		JobID:    j.GetId(),
		Status:   j.GetStatus(),
		ExitCode: int(j.GetExitCode()),
		Stdout:   j.GetStdout(),
		Stderr:   j.GetStderr(),
		Error:    j.GetError(),
	}
}

// bearerUnary attaches the Bearer token to every unary RPC. When the
// caller's ctx already has an `authorization` metadata entry, it
// wins — useful for tests injecting a different identity.
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
