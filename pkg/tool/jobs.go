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
	Bin    string            `json:"bin" jsonschema:"description=BIN name as declared in this agent's Agentfile (e.g. sh, jq, kubectl)"`
	Args   []string          `json:"args,omitempty" jsonschema:"description=Positional arguments forwarded to the BIN"`
	Stdin  string            `json:"stdin,omitempty" jsonschema:"description=Optional stdin payload piped to the BIN"`
	Labels map[string]string `json:"labels,omitempty" jsonschema:"description=Optional metadata; reserved keys live under io.openotters.* (session-id, origin, …)"`
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
			"Submit a long-running BIN job against this agent's spawn env. Returns a job_id immediately; use job_status to poll or job_wait to block until completion. Prefer over directly invoking a BIN when the work is long-running, when you want operator-visible progress, or when you want to fire-and-forget.",
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
			"Non-blocking snapshot of an async job's state. Returns status (pending/running/done/error/cancelled/orphaned), exit_code, stdout, stderr, error. Use this when you want to check on a job without blocking your turn.",
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
			"Block until an async job reaches a terminal status (done/error/cancelled/orphaned), then return its final state. If your turn is interrupted, the job is automatically cancelled — no orphans. Prefer job_status if you want to check without blocking.",
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
