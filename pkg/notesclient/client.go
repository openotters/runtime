// Package notesclient is the runtime-side client to the daemon's
// agent_notes table. Mirrors the legacy pkg/notes.Store API so the
// tool layer and PrepareStep renderer can swap stores for clients
// without changing call sites.
//
// Configuration mirrors pkg/jobsclient: OTTERSD_URL + OTTERS_AGENT_TOKEN
// from the spawn env. The daemon scopes every call to the JWT's
// agent_ref, so the client never needs to know its own UUID — empty
// ref on the wire makes the daemon read it from the bearer token.
package notesclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	daemonv1 "github.com/openotters/openotters/api/v1"

	"github.com/openotters/runtime/pkg/notes"
)

// Note re-exports notes.Note so callers can write notesclient.Note
// without importing both packages. Same in-memory shape; same
// renderers; only the storage moved.
type Note = notes.Note

// Store is the interface BuildNotesTools / BuildNotesPrepareStep
// need from a notes backend. *Client satisfies it in production;
// *Fake satisfies it in tests.
type Store interface {
	List(ctx context.Context) ([]notes.Note, error)
	ListInContext(ctx context.Context) ([]notes.Note, error)
	Get(ctx context.Context, key string) (notes.Note, error)
	Save(ctx context.Context, key, content string, maxBytes, maxCount int) error
	Delete(ctx context.Context, key string) error
	SetInContext(ctx context.Context, key string, inContext bool) error
}

const (
	EnvDaemonURL = "OTTERSD_URL"
	//nolint:gosec // G101: env var name, not a credential value
	EnvAgentToken = "OTTERS_AGENT_TOKEN"
)

// Sentinel errors. Returned with %w-wrapping so callers can
// errors.Is to render meaningful messages.
var (
	ErrInvalidKey    = errors.New("invalid note key")
	ErrNoteTooLarge  = errors.New("note content exceeds size cap")
	ErrTooManyNotes  = errors.New("note count would exceed cap")
	ErrNoteNotFound  = errors.New("note not found")
	ErrNotConfigured = errors.New(
		"notesclient: OTTERSD_URL and OTTERS_AGENT_TOKEN must both be set",
	)
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
		c.err = fmt.Errorf("notesclient: dial %s: %w", target, err)
		return
	}
	c.conn = conn
	c.rt = daemonv1.NewAgentStateClient(conn)
}

func (c *Client) ensure() error {
	c.once.Do(c.dial)
	return c.err
}

// List mirrors notes.Store.List — every note ordered by
// most-recently-updated first. Body is omitted to keep payloads
// small; callers fetch the body via Get.
func (c *Client) List(ctx context.Context) ([]Note, error) {
	return c.list(ctx, false)
}

// ListInContext mirrors notes.Store.ListInContext.
func (c *Client) ListInContext(ctx context.Context) ([]Note, error) {
	return c.list(ctx, true)
}

func (c *Client) list(ctx context.Context, onlyInContext bool) ([]Note, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}
	resp, err := c.rt.ListNotes(ctx, &daemonv1.StateListNotesRequest{
		OnlyInContext: onlyInContext,
	})
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]Note, 0, len(resp.GetNotes()))
	for _, n := range resp.GetNotes() {
		out = append(out, noteFromProto(n))
	}
	return out, nil
}

// Get returns one note by key.
func (c *Client) Get(ctx context.Context, key string) (Note, error) {
	if err := c.ensure(); err != nil {
		return Note{}, err
	}
	resp, err := c.rt.GetNote(ctx, &daemonv1.StateGetNoteRequest{Key: key})
	if err != nil {
		return Note{}, mapError(err)
	}
	return noteFromProto(resp.GetNote()), nil
}

// Save upserts a note. maxBytes / maxCount enforce quotas; pass
// the agent.yaml-configured values. The daemon returns
// FailedPrecondition for quota violations, mapped here to
// ErrNoteTooLarge / ErrTooManyNotes.
func (c *Client) Save(ctx context.Context, key, content string, maxBytes, maxCount int) error {
	if err := c.ensure(); err != nil {
		return err
	}
	_, err := c.rt.SaveNote(ctx, &daemonv1.StateSaveNoteRequest{
		Key:      key,
		Content:  content,
		MaxBytes: int32(maxBytes), //nolint:gosec // caller-bounded
		MaxCount: int32(maxCount), //nolint:gosec // caller-bounded
	})
	return mapError(err)
}

// Delete removes a note by key. Idempotent — no error when the
// key was never present.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.ensure(); err != nil {
		return err
	}
	_, err := c.rt.DeleteNote(ctx, &daemonv1.StateDeleteNoteRequest{Key: key})
	return mapError(err)
}

// SetInContext flips a note's in_context flag.
func (c *Client) SetInContext(ctx context.Context, key string, inContext bool) error {
	if err := c.ensure(); err != nil {
		return err
	}
	_, err := c.rt.SetNoteInContext(ctx, &daemonv1.StateSetNoteInContextRequest{
		Key:       key,
		InContext: inContext,
	})
	return mapError(err)
}

func noteFromProto(n *daemonv1.NoteRow) Note {
	if n == nil {
		return Note{}
	}
	return Note{
		Key:       n.GetKey(),
		Content:   n.GetContent(),
		Preview:   n.GetPreview(),
		InContext: n.GetInContext(),
		CreatedAt: time.Unix(n.GetCreatedUnix(), 0),
		UpdatedAt: time.Unix(n.GetUpdatedUnix(), 0),
	}
}

// mapError translates daemon-side Connect codes back into the
// local sentinel errors so callers built against pkg/notes don't
// have to learn the wire shape.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return err
	}
	switch ce.Code() { //nolint:exhaustive // map known codes; pass others through
	case connect.CodeNotFound:
		return fmt.Errorf("%w: %s", ErrNoteNotFound, ce.Message())
	case connect.CodeInvalidArgument:
		return fmt.Errorf("%w: %s", ErrInvalidKey, ce.Message())
	case connect.CodeFailedPrecondition:
		msg := ce.Message()
		if strings.Contains(msg, "size cap") || strings.Contains(msg, "bytes") {
			return fmt.Errorf("%w: %s", ErrNoteTooLarge, msg)
		}
		return fmt.Errorf("%w: %s", ErrTooManyNotes, msg)
	}
	return err
}

func dialTarget(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("notesclient: parse OTTERSD_URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" && u.Opaque != "" {
			path = u.Opaque
		}
		if path == "" {
			return "", fmt.Errorf("notesclient: OTTERSD_URL %q has no socket path", raw)
		}
		return "unix:" + path, nil
	case "http":
		host := u.Host
		if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
			host = net.JoinHostPort(host, "80")
		}
		return host, nil
	default:
		return "", fmt.Errorf("notesclient: OTTERSD_URL %q: unsupported scheme %q", raw, u.Scheme)
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
