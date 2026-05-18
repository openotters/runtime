// markdown the model reads in its tool catalogue. Breaking the
// prose at 120 chars with Go string concatenation actively hurts
// readability for both reviewers and the model (which sees the
// rendered string). Same precedent as pkg/tool/jobs.go.
//
//nolint:lll // The four tool descriptions below are multi-paragraph
package tool

// agents.go registers the three LLM-facing agent-to-agent tools:
//
//   - agent_list : enumerate agents I'm linked to (= can call)
//   - agent_info : inspect one linked agent's metadata + caps
//   - agent_exec : send a prompt to a linked agent and wait for
//                  the full reply. Pass session_id to preserve
//                  history on the target across calls; omit it
//                  for a fresh thread.
//
// All three go through the daemon via OTTERSD_URL +
// OTTERS_AGENT_TOKEN — the same channel the job tools use. The
// daemon authorises each call against the JWT's Links claim;
// trying to call an unlinked agent surfaces as an IsError tool
// response the model can read.
//
// (alpha.85 dropped a separate `agent_chat` tool that did the
// threaded variant; merged into agent_exec via the new session_id
// parameter to keep the surface tight.)

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/openotters/runtime/pkg/agentclient"
)

// BuildAgentTools returns the three agent_* tools when a daemon
// callback path is wired. Empty slice when not — same gating shape
// the job tools use.
//
// One shared *agentclient.Client across all three; the underlying
// gRPC connection multiplexes RPCs and lazy-dials on first use.
func BuildAgentTools() []fantasy.AgentTool {
	cfg, ok := agentclient.FromEnv()
	if !ok {
		return nil
	}
	client := agentclient.New(cfg)

	return []fantasy.AgentTool{
		agentListTool(client),
		agentInfoTool(client),
		agentExecTool(client),
	}
}

// agentListInput is empty — agent_list returns every link the
// caller has, no filters. The model uses this as the index
// before any other agent_* call.
type agentListInput struct{}

// agentRefInput is the shared "by ref" shape for tools that
// target one linked agent.
type agentRefInput struct {
	Ref string `json:"ref" jsonschema:"description=Name or id of a linked agent (as returned by agent_list)"`
}

// agentChatInput threads a session id so multi-turn dialogues
// with one target preserve memory.
type agentChatInput struct {
	Ref       string `json:"ref" jsonschema:"description=Name or id of a linked agent"`
	Prompt    string `json:"prompt" jsonschema:"description=Message to send. The target sees it as a single user turn."`
	SessionID string `json:"session_id,omitempty" jsonschema:"description=Optional. Empty creates a fresh session; pass back the returned session_id for follow-ups."`
}

func agentListTool(c *agentclient.Client) fantasy.AgentTool {
	desc := `List every agent you can call. Returns id, name, model, and live status.

WHEN TO USE:
  • At the start of any task you might delegate — see who's available before asking the user.
  • Whenever you're tempted to do something outside your specialty (e.g. you're a HASS agent and the user asked about Kubernetes — agent_list might show a kubectl agent you can hand off to).

This list is your call graph: every entry here is callable via agent_chat / agent_exec / agent_info. Names NOT in this list are unreachable; the daemon will reject calls to them.`
	return fantasy.NewAgentTool(
		"agent_list",
		desc,
		func(ctx context.Context, _ agentListInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			agents, err := c.AgentList(ctx)
			if err != nil {
				return errToolResp(err)
			}
			if len(agents) == 0 {
				return fantasy.ToolResponse{
					Content: "You are not linked to any other agents. Tell the user — don't try to call one.",
				}, nil
			}
			var b strings.Builder
			b.WriteString("| Name | Model | Status |\n")
			b.WriteString("|------|-------|--------|\n")
			for _, a := range agents {
				fmt.Fprintf(&b, "| `%s` | %s | %s |\n", a.Name, a.Model, a.Status)
			}
			return fantasy.ToolResponse{Content: b.String()}, nil
		},
	)
}

func agentInfoTool(c *agentclient.Client) fantasy.AgentTool {
	desc := `Inspect one linked agent: model, status, description, and the full list of tool capabilities. Call this BEFORE agent_chat / agent_exec when you're not sure whether a given agent can handle the task you'd delegate to it.

EXAMPLE:
  agent_info({"ref":"Homelab"})
  → {"name":"Homelab", "model":"...", "description":"Operate a Kubernetes cluster", "capabilities":["sh","kubectl","jq",...]}

Errors with PermissionDenied if the ref isn't in your link set.`
	return fantasy.NewAgentTool(
		"agent_info",
		desc,
		func(ctx context.Context, in agentRefInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			ref := strings.TrimSpace(in.Ref)
			if ref == "" {
				return fantasy.ToolResponse{
					IsError: true,
					Content: "ref is required (call agent_list first to see options)",
				}, nil
			}
			info, err := c.AgentInfo(ctx, ref)
			if err != nil {
				return errToolResp(err)
			}
			payload, err := json.Marshal(info)
			if err != nil {
				return errToolResp(err)
			}
			return fantasy.ToolResponse{Content: string(payload)}, nil
		},
	)
}

func agentExecTool(c *agentclient.Client) fantasy.AgentTool {
	desc := `**Delegate a request to a linked agent and wait for its full reply.** The target sees your prompt as a single user turn and persists it.

EXAMPLE — fresh thread:
  agent_exec({"ref":"Homelab", "prompt":"List namespaces in the kube-system tier"})
  → {"response":"...", "session_id":"from-agent:<your-id>:abc-123"}

EXAMPLE — continue an existing thread (preserve history on the target):
  agent_exec({"ref":"Homelab", "prompt":"How many pods in each?", "session_id":"from-agent:<your-id>:abc-123"})

WHEN TO PASS session_id:
  • The follow-up depends on what you asked before (you don't want to re-state the context).
  • You're orchestrating a multi-turn collaboration with the same target.

WHEN TO OMIT session_id:
  • The task is single-turn and standalone.
  • You explicitly want a fresh conversational state on the target.

BLOCKING. This tool returns when the target finishes its turn.

Errors with PermissionDenied if ref isn't in your link set.`
	return fantasy.NewAgentTool(
		"agent_exec",
		desc,
		func(ctx context.Context, in agentChatInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			ref := strings.TrimSpace(in.Ref)
			if ref == "" || strings.TrimSpace(in.Prompt) == "" {
				return fantasy.ToolResponse{
					IsError: true,
					Content: "ref and prompt are both required",
				}, nil
			}
			resp, sessionID, err := c.AgentExec(ctx, ref, in.Prompt, in.SessionID)
			if err != nil {
				return errToolResp(err)
			}
			payload, err := json.Marshal(struct {
				Response  string `json:"response"`
				SessionID string `json:"session_id"`
			}{Response: resp, SessionID: sessionID})
			if err != nil {
				return errToolResp(err)
			}
			return fantasy.ToolResponse{Content: string(payload)}, nil
		},
	)
}
