package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"

	"github.com/openotters/runtime/pkg/jobsclient"
)

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
				id, err := client.SubmitJob(ctx, in.Bin, in.Args, in.Stdin, in.Labels)
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
