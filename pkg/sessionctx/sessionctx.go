// Package sessionctx threads the current chat session id through the
// per-call context.Context so tool callbacks can access it without
// the framework having to pass it explicitly through every layer.
//
// Sessions are per-chat-turn, not per-process — the runtime hosts
// many sessions over its lifetime — so env vars are the wrong
// channel. Putting the value on ctx mirrors how cancellation,
// deadlines, and tracing IDs already flow.
//
// Producers: pkg/agent/service.go's ChatStream wraps the ctx before
// invoking the model loop.
// Consumers: tool callbacks pull the session id when they want to
// stamp it onto outbound work (e.g. job_submit attaches it as the
// io.openotters.session-id label so /jobs filtering by session
// works).
package sessionctx

import "context"

// ctxKey keeps the lookup key unexported — callers can't stash
// arbitrary strings under our key by accident.
type ctxKey struct{}

// With returns a derived ctx carrying sessionID. Empty input is a
// no-op (returns the parent ctx) — keeps producer call sites from
// having to guard.
func With(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, sessionID)
}

// From returns the session id attached by With, or "" when the ctx
// carries none. Tool callbacks treat "" as "no session is in
// scope" — they should NOT fall back to env vars or the agent's
// own id; absence is meaningful.
func From(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}
