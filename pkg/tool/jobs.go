package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"

	"github.com/openotters/runtime/pkg/jobsclient"
	"github.com/openotters/runtime/pkg/sessionctx"
)

// labelSessionID is the standard reserved label for "the chat session
// this job was originated from." Mirrors the constant the daemon docs
// in api/v1/daemon.proto. Centralised here so the auto-stamp logic
// has one place to look.
const labelSessionID = "io.openotters.session-id"

// SubmitJobInput is the model-facing schema for `job_submit`. The
// jsonschema tags drive what the model sees in its tool-list prompt
// — keep them tight so the model isn't tempted to pass irrelevant
// fields.
type SubmitJobInput struct {
	Bin    string            `json:"bin" jsonschema:"description=BIN name as declared in this agent's Agentfile (sh, jq, kubectl, …)"`
	Args   []string          `json:"args,omitempty" jsonschema:"description=Positional arguments forwarded to the BIN"`
	Stdin  string            `json:"stdin,omitempty" jsonschema:"description=Optional stdin payload piped to the BIN"`
	Labels map[string]string `json:"labels,omitempty" jsonschema:"description=Optional metadata for filtering in /jobs (rare; leave empty unless an operator asked for it)"`
}

// JobIDInput is the shared schema for status / wait / cancel — they
// all take just a job id.
type JobIDInput struct {
	JobID string `json:"job_id" jsonschema:"description=Job id returned by job_submit"`
}

// ListJobsInput controls the agent-side job_list scope. By default
// only jobs from the current chat session are returned (matching
// the io.openotters.session-id label that job_submit auto-stamps);
// flip AllSessions to see everything this agent has submitted across
// sessions. Status / Limit are additional narrowers. AgentID is
// never accepted — the daemon scopes by the agent's bound JWT, so
// the agent can never list another agent's jobs.
type ListJobsInput struct {
	AllSessions bool   `json:"all_sessions,omitempty" jsonschema:"description=List jobs across all your sessions. Default false — only this chat session."`
	Status      string `json:"status,omitempty" jsonschema:"description=Filter by status: pending, running, done, error, cancelled, orphaned. Empty = all."`
	Limit       int    `json:"limit,omitempty" jsonschema:"description=Maximum number of jobs to return (most recent first). Default 20, max 100."`
}

// BuildJobTools returns the three async-job tools when a daemon
// callback path is wired (OTTERSD_URL + OTTERS_AGENT_TOKEN both set
// in the spawn env). Returns an empty slice when env vars are
// missing — caller appends to LoadTools' result so absence just
// means the agent doesn't see those tools, not a hard failure.
//
// One *jobsclient.Client is shared across all three tools — the
// underlying gRPC connection multiplexes RPCs and lazy-dials on
// first use, so an agent that never invokes a job tool pays
// nothing.
func BuildJobTools() []fantasy.AgentTool {
	cfg, ok := jobsclient.FromEnv()
	if !ok {
		return nil
	}
	client := jobsclient.New(cfg)

	return []fantasy.AgentTool{
		fantasy.NewAgentTool(
			"job_submit",
			`Submit a BIN to run in the background. Returns a job_id immediately so you can continue while it runs.

WHEN TO USE THIS instead of invoking the BIN directly:
  • The command may take more than ~10 seconds (kubectl rollout, build, install, large query)
  • You want to start it and keep thinking / do other work in the meantime
  • You want operator-visible progress (the job shows in the /jobs dashboard)
  • You want fire-and-forget — no need to wait for the result

OTHERWISE invoke the BIN directly (kubectl, jq, sh, …). Faster, no daemon round-trip — the right default for quick reads, single-step commands, and most jq / cat / grep work.

EXAMPLES:
  Long kubectl rollout:
    job_submit({"bin":"kubectl","args":["rollout","status","deploy/api","--timeout=5m"]})
    → {"job_id":"job_abc123"}

  sh pipeline that takes time:
    job_submit({"bin":"sh","args":["-c","find / -name '*.log' | xargs gzip -k"]})
    → {"job_id":"job_def456"}

  Quick "kubectl get pods" — DO NOT use this; call kubectl directly:
    kubectl({"args":["get","pods"]})         ✓ right tool — fast, sync
    job_submit({"bin":"kubectl",...})        ✗ wrong — wasteful round-trip

After submitting, use job_wait(job_id) to block until done, or job_status(job_id) to peek.`,
			func(ctx context.Context, in SubmitJobInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				if in.Bin == "" {
					return fantasy.ToolResponse{
						IsError: true,
						Content: "job_submit: bin is required",
					}, nil
				}
				// Auto-stamp io.openotters.session-id from the chat
				// session in scope. Lets operators filter /jobs by
				// "show me everything from this conversation" with
				// zero model effort. Model-supplied label wins if it
				// passes one explicitly — explicit > inferred.
				labels := in.Labels
				if sess := sessionctx.From(ctx); sess != "" {
					if labels == nil {
						labels = map[string]string{}
					}
					if _, has := labels[labelSessionID]; !has {
						labels[labelSessionID] = sess
					}
				}
				id, err := client.SubmitJob(ctx, in.Bin, in.Args, in.Stdin, labels)
				if err != nil {
					return fantasy.ToolResponse{
						IsError: true,
						Content: fmt.Sprintf("job_submit failed: %s", err),
					}, nil
				}
				return fantasy.ToolResponse{
					Content: fmt.Sprintf(`{"job_id":%q}`, id),
				}, nil
			},
		),

		fantasy.NewAgentTool(
			"job_status",
			`Non-blocking snapshot of a job's state. Returns status (pending|running|done|error|cancelled|orphaned), exit_code, stdout, stderr, error. Returns immediately even if the job is still running.

USE THIS to peek at a previously-submitted job without waiting — "is the rollout done yet?" mid-conversation, or after the operator has handed you a job_id from elsewhere.

DON'T USE THIS in a polling loop. If you want to block until completion, call job_wait once — it handles the wait efficiently with no busy-loop, and forwards interruption cleanly.

EXAMPLES:
  Peek at an in-flight rollout while doing something else:
    job_status({"job_id":"job_abc123"})
    → {"job_id":"job_abc123","status":"running","exit_code":0,"stdout":"","stderr":""}

  Same job after it finished:
    job_status({"job_id":"job_abc123"})
    → {"job_id":"job_abc123","status":"done","exit_code":0,"stdout":"deployment \"api\" successfully rolled out\n","stderr":""}

  Wrong: polling in a loop — use job_wait instead.
    while not done: job_status(...)         ✗ wastes turns
    job_wait({"job_id":"..."})              ✓ one call, one block`,
			func(ctx context.Context, in JobIDInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				if in.JobID == "" {
					return fantasy.ToolResponse{
						IsError: true,
						Content: "job_status: job_id is required",
					}, nil
				}
				view, err := client.GetJob(ctx, in.JobID)
				if err != nil {
					return fantasy.ToolResponse{
						IsError: true,
						Content: fmt.Sprintf("job_status failed: %s", err),
					}, nil
				}
				return jobViewResponse(view), nil
			},
		),

		fantasy.NewAgentTool(
			"job_list",
			`List your recent async jobs. By default scoped to the current chat session — set all_sessions=true to see every job you've submitted regardless of session. Always agent-scoped: you can never see another agent's jobs, the daemon enforces this with the JWT regardless of what's in the request.

USE THIS when the operator asks "what did I just run", "show me my jobs", "is anything still running", or you need to recover a job_id you didn't remember to keep.

DON'T USE THIS to poll a single job — use job_status / job_wait instead, both narrower and cheaper.

EXAMPLES:
  Default — recent jobs in this session:
    job_list({})
    → [{"job_id":"job_abc","status":"running","exit_code":0,"stdout":"","stderr":""}, …]

  All my jobs across all sessions, only the ones still running:
    job_list({"all_sessions":true,"status":"running"})

  My last 5 jobs (this session):
    job_list({"limit":5})`,
			func(ctx context.Context, in ListJobsInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				opts := jobsclient.ListJobsOpts{Status: in.Status}

				// Default: scope to the current chat session via the
				// auto-stamped label. AllSessions=true clears the scope
				// — agent decides whether the broader view is useful.
				if !in.AllSessions {
					if sess := sessionctx.From(ctx); sess != "" {
						opts.Labels = map[string]string{labelSessionID: sess}
					}
				}

				jobs, err := client.ListJobs(ctx, opts)
				if err != nil {
					return fantasy.ToolResponse{
						IsError: true,
						Content: fmt.Sprintf("job_list failed: %s", err),
					}, nil
				}

				// Apply the agent-side limit AFTER fetch. The daemon
				// doesn't expose a server-side limit on this RPC yet;
				// for typical session-scoped queries the result set is
				// already small, so paginating here is fine. Default
				// 20 keeps the model's tool-output budget tight.
				limit := in.Limit
				if limit <= 0 {
					limit = 20
				}
				if limit > 100 {
					limit = 100
				}
				if len(jobs) > limit {
					jobs = jobs[:limit]
				}

				body, mErr := json.Marshal(jobs)
				if mErr != nil {
					return fantasy.ToolResponse{
						IsError: true,
						Content: fmt.Sprintf("encoding jobs: %s", mErr),
					}, nil
				}

				return fantasy.ToolResponse{Content: string(body)}, nil
			},
		),

		fantasy.NewAgentTool(
			"job_cancel",
			`Cancel an in-flight async job. The daemon SIGKILLs the underlying BIN process (0s grace) and transitions the row to status="cancelled" once the executor reaps it (~250 ms typical). Stdout/stderr captured up to the kill point are preserved.

USE THIS when a previously-submitted job is no longer wanted — operator changed their mind, an upstream condition made the work redundant, or the BIN is misbehaving and needs to stop now.

NO-OP behaviour: cancelling a job that is already terminal (done|error|cancelled|orphaned) returns an error from the daemon. That's not a failure of intent — it just means the job already finished. Read the error message; don't retry.

EXAMPLES:
  Cancel a runaway rollout:
    job_cancel({"job_id":"job_abc123"})
    → {"job_id":"job_abc123","cancelled":true}

  Cancel + check the captured output:
    job_cancel({"job_id":"job_abc"})
    job_status({"job_id":"job_abc"})
    → {"job_id":"job_abc","status":"cancelled","exit_code":0,"stdout":"…partial bytes…","stderr":""}

  Already-finished job:
    job_cancel({"job_id":"job_done"})
    → tool error: "async job not currently running" — job already terminal, nothing to cancel.`,
			func(ctx context.Context, in JobIDInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				if in.JobID == "" {
					return fantasy.ToolResponse{
						IsError: true,
						Content: "job_cancel: job_id is required",
					}, nil
				}
				if err := client.CancelJob(ctx, in.JobID); err != nil {
					return fantasy.ToolResponse{
						IsError: true,
						Content: fmt.Sprintf("job_cancel failed: %s", err),
					}, nil
				}
				return fantasy.ToolResponse{
					Content: fmt.Sprintf(`{"job_id":%q,"cancelled":true}`, in.JobID),
				}, nil
			},
		),

		fantasy.NewAgentTool(
			"job_wait",
			`Block until a job reaches a terminal status (done|error|cancelled|orphaned), then return its final state — stdout, stderr, exit_code, and any error. Efficient: no busy-loop, no extra turns burned.

USE THIS when you submitted a job and need the result before continuing.

WHEN NOT TO USE: if you can do other useful work while the job runs, call job_status to peek instead. Calling job_wait throws away the async benefit — equivalent to making the call synchronously in the first place.

SAFETY: if your turn is interrupted while you're waiting, the job is auto-cancelled on the daemon — no orphans, no surprise charges.

EXAMPLES:
  Submit then wait — common "long-running call I need the result of":
    {"job_id":"job_abc"} = job_submit({"bin":"kubectl","args":["rollout","status","deploy/api"]})
    job_wait({"job_id":"job_abc"})
    → {"job_id":"job_abc","status":"done","exit_code":0,"stdout":"successfully rolled out\n","stderr":""}

  Wait on a job the operator handed you:
    job_wait({"job_id":"job_def456"})
    → {...final state...}`,
			func(ctx context.Context, in JobIDInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				if in.JobID == "" {
					return fantasy.ToolResponse{
						IsError: true,
						Content: "job_wait: job_id is required",
					}, nil
				}
				view, err := client.WatchJob(ctx, in.JobID)
				if err != nil {
					return fantasy.ToolResponse{
						IsError: true,
						Content: fmt.Sprintf("job_wait failed: %s", err),
					}, nil
				}
				return jobViewResponse(view), nil
			},
		),

		fantasy.NewAgentTool(
			"job_watch",
			`Watch a job until it reaches terminal status and return ONLY its stdout — no exit code, no stderr, no status framing. Treats the job like a pipe you're tailing.

DIFFERENCE FROM job_wait — this is the load-bearing one:
- job_wait → blocks for the full state on completion; if YOUR turn is interrupted, the daemon cancels the underlying job (so you don't strand work you no longer want).
- job_watch → blocks for the stdout on completion; if YOUR turn is interrupted, the job KEEPS RUNNING on the daemon — you just stop observing. Pick this when you're following work that's not yours to kill, or when you want the model to be able to detach cheaply.

USE THIS when you only care about the BIN's stdout — feeding it to another tool, returning it as the final answer, summarising it. You lose visibility on exit_code / stderr / error, so call job_status afterward if you need framing.

DON'T USE THIS when you need the exit code to branch on success/failure — call job_wait, which surfaces that.

EXAMPLES:
  Get just the rolled-out manifest:
    job_watch({"job_id":"job_abc"})
    → "successfully rolled out\nrevision 12 ready\n"

  Already-finished job — same shape, returns its captured stdout:
    job_watch({"job_id":"job_done"})
    → "final output line\n"`,
			func(ctx context.Context, in JobIDInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				if in.JobID == "" {
					return fantasy.ToolResponse{
						IsError: true,
						Content: "job_watch: job_id is required",
					}, nil
				}
				view, err := client.FollowJob(ctx, in.JobID)
				if err != nil {
					// Partial snapshot may be non-nil if we got
					// SOME state before the error. Surface what we
					// have alongside an IsError so the model can
					// reason about an interrupted observation.
					content := fmt.Sprintf("job_watch failed: %s", err)
					if view != nil {
						content = view.Stdout + "\n\n[job_watch interrupted: " + err.Error() + "]"
					}
					return fantasy.ToolResponse{
						IsError: true,
						Content: content,
					}, nil
				}
				return fantasy.ToolResponse{Content: view.Stdout}, nil
			},
		),
	}
}

// jobViewResponse encodes a JobView as the tool's content body. JSON
// because it's the natural format the model reads back; no
// IsError flag because a job that completed with exit_code != 0 is
// NOT a tool error — the BIN ran, the model needs to see the
// outcome and decide. Hard tool errors (network, unauthorized) are
// surfaced via IsError separately.
func jobViewResponse(v *jobsclient.JobView) fantasy.ToolResponse {
	body, err := json.Marshal(v)
	if err != nil {
		return fantasy.ToolResponse{
			IsError: true,
			Content: fmt.Sprintf("encoding job view: %s", err),
		}
	}
	return fantasy.ToolResponse{Content: string(body)}
}
