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
		agentCreateTool(client),
		agentCreateFromSourceTool(client),
		agentDeleteTool(client),
		imageListTool(client),
		binListTool(client),
		selfReloadTool(client),
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

// agentCreateInput mirrors agentclient.AgentCreateInput on the wire
// so the JSON schema fantasy generates surfaces field-by-field
// descriptions to the model. Same shape, jsonschema tags added.
type agentCreateInput struct {
	Ref         string            `json:"ref" jsonschema:"description=Image ref to spawn from (e.g. kubectl:latest). Use image_list to discover available refs."`
	Name        string            `json:"name,omitempty" jsonschema:"description=Optional name for the new agent. Daemon auto-generates one if empty."`
	Model       string            `json:"model,omitempty" jsonschema:"description=Optional model override (e.g. anthropic/claude-opus-4-7). Falls back to the image's MODEL directive."`
	Envs        map[string]string `json:"envs,omitempty" jsonschema:"description=Per-run ENV overrides. Reserved keys (PATH, HOME, *_API_KEY) are rejected by the daemon."`
	Links       []string          `json:"links,omitempty" jsonschema:"description=INBOUND links to the new agent — each entry is an agent ref (id or name) that can call the new agent. Include your own ref to gain call permission yourself, then call self_reload to refresh your JWT."`
	Description string            `json:"description,omitempty" jsonschema:"description=One-line description shown to the operator + future callers. Stored as the description label."`
}

type agentCreateFromSourceInput struct {
	Agentfile   string            `json:"agentfile" jsonschema:"description=Raw Agentfile body (FROM / MODEL / SOUL / BIN / ENV / etc.). Must be self-contained — no COPY-from-host."`
	Name        string            `json:"name,omitempty" jsonschema:"description=Optional name for the new agent."`
	Model       string            `json:"model,omitempty" jsonschema:"description=Optional model override."`
	Envs        map[string]string `json:"envs,omitempty" jsonschema:"description=Per-run ENV overrides."`
	Links       []string          `json:"links,omitempty" jsonschema:"description=INBOUND links — same semantics as agent_create.links."`
	Description string            `json:"description,omitempty" jsonschema:"description=One-line description."`
}

// agentCreateTool and agentCreateFromSourceTool deliberately share
// the same validate / call / marshal skeleton — the input type and
// the client method are what differ. Collapsing them into a generic
// helper would obscure the fact that the daemon enforces two
// different capabilities behind these tools.
//
//nolint:dupl // shape parallel is intentional; see comment above.
func agentCreateTool(c *agentclient.Client) fantasy.AgentTool {
	desc := `Spawn a new agent from an existing image ref. Returns id / name / status.

EXAMPLE:
  agent_create({"ref":"kubectl:latest","name":"scratch-kube","description":"one-off namespace scan","links":["<your-id>"]})
  → {"id":"...","name":"scratch-kube","status":"pulling"}

WORKFLOW — when you want to delegate to a freshly-spawned agent:
  1. image_list → find a ref that fits.
  2. agent_create with links=[<your-id>] so YOU gain call permission on the new agent.
  3. self_reload as the LAST tool of THIS turn — your in-memory JWT still carries the pre-link claim set; this restarts your runtime so the new link takes effect.
  4. On a later turn: agent_exec({"ref":"<new-name>", ...}) to send the actual task. The new agent is now callable.
  5. agent_delete when the task is done.

LINKS ARE INBOUND. Each ref in "links" is a SOURCE that gains permission to call the new agent (not the other way around). If you don't include your own ref, you spawn an agent you can't delegate to.

CONSTRAINTS:
  • Mounts are not supported — host filesystem access stays operator-only.
  • For an image that doesn't exist yet, use agent_create_from_source with an Agentfile body.`
	return fantasy.NewAgentTool(
		"agent_create",
		desc,
		func(ctx context.Context, in agentCreateInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(in.Ref) == "" {
				return fantasy.ToolResponse{
					IsError: true,
					Content: "ref is required (call image_list to see available agent images)",
				}, nil
			}
			res, err := c.AgentCreate(ctx, agentclient.AgentCreateInput{
				Ref: in.Ref, Name: in.Name, Model: in.Model,
				Envs: in.Envs, Links: in.Links, Description: in.Description,
			})
			if err != nil {
				return errToolResp(err)
			}
			payload, err := json.Marshal(res)
			if err != nil {
				return errToolResp(err)
			}
			return fantasy.ToolResponse{Content: string(payload)}, nil
		},
	)
}

//nolint:dupl // shape parallel with agentCreateTool is intentional.
func agentCreateFromSourceTool(c *agentclient.Client) fantasy.AgentTool {
	desc := `Build a fresh image from an inline Agentfile body, then spawn an agent from it. The daemon tags the generated image under "from-agent-<your-id>:<uuid>" so the provenance is visible in image_list.

AGENTFILE SYNTAX (the exact grammar the daemon parses):

  FROM <ref>                       # required. "scratch" for an empty base.
  RUNTIME <ref>                    # OCI ref carrying the runtime binary
  MODEL <provider/model>           # e.g. anthropic-local/claude-opus-4-7
  NAME <agent-name>                # optional; daemon assigns one otherwise

  LABEL <key>="<value>"            # one per line; quote the value

  CONTEXT <NAME> [<description>] <<EOF
  ... the SOUL or other context body, any number of lines ...
  EOF

  BIN <alias> <bin-ref>            # one per BIN — alias is what the model invokes
  ENV <KEY>=<value>                # optional ENV (uppercase, no PATH/HOME/*_API_KEY)

Heredoc (<<EOF ... EOF) IS supported for CONTEXT bodies — that's how SOUL prompts are passed. The keyword is CONTEXT <NAME>, not bare SOUL. Use CONTEXT SOUL for the operating-rules block.

EXAMPLE:
  agent_create_from_source({
    "agentfile": "FROM scratch\nRUNTIME ghcr.io/openotters/runtime:latest\nMODEL anthropic-local/claude-opus-4-7\nNAME meteo\n\nCONTEXT SOUL <<EOF\nYou are a weather specialist. Use curl to hit api.open-meteo.com.\nEOF\n\nBIN curl ghcr.io/openotters/tools/curl:latest\nBIN jq ghcr.io/openotters/tools/jq:latest\n",
    "name": "Meteo",
    "links": ["<your-id>"]
  })
  → {"id":"...","name":"Meteo","status":"pulling"}

USE WHEN:
  • You need a specific BIN combination that no existing image carries — bin_list shows what's available; FROM / BIN refs are resolved against the local registry.
  • You're composing a one-off specialist for a specific task and don't want to publish a reusable image.

CONSTRAINTS:
  • The Agentfile body must be self-contained — no COPY-from-host, no file uploads.
  • The generated image persists until the operator removes it. Use agent_delete on the agent when done; the image is the operator's to garbage-collect.
  • Same field semantics as agent_create otherwise — links is INBOUND; include your own ref + call self_reload to delegate to the new agent.`
	return fantasy.NewAgentTool(
		"agent_create_from_source",
		desc,
		func(ctx context.Context, in agentCreateFromSourceInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(in.Agentfile) == "" {
				return fantasy.ToolResponse{
					IsError: true,
					Content: "agentfile body is required",
				}, nil
			}
			res, err := c.AgentCreateFromSource(ctx, agentclient.AgentCreateFromSourceInput{
				Agentfile: in.Agentfile, Name: in.Name, Model: in.Model,
				Envs: in.Envs, Links: in.Links, Description: in.Description,
			})
			if err != nil {
				return errToolResp(err)
			}
			payload, err := json.Marshal(res)
			if err != nil {
				return errToolResp(err)
			}
			return fantasy.ToolResponse{Content: string(payload)}, nil
		},
	)
}

func agentDeleteTool(c *agentclient.Client) fantasy.AgentTool {
	desc := `Delete an agent by name or id. Returns silently on success.

NO PERMISSION CHECK: any agent can be deleted, including operator-created ones. Use with discipline:
  • Default to deleting only agents you yourself spawned via agent_create / agent_create_from_source.
  • Never delete an operator-created agent without an explicit instruction from the operator.

EXAMPLE:
  agent_delete({"ref":"scratch-kube"})

Self-delete is allowed at the RPC layer but pointless — you'd lose access to the daemon on your next call.`
	return fantasy.NewAgentTool(
		"agent_delete",
		desc,
		func(ctx context.Context, in agentRefInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			ref := strings.TrimSpace(in.Ref)
			if ref == "" {
				return fantasy.ToolResponse{
					IsError: true,
					Content: "ref is required",
				}, nil
			}
			if err := c.AgentDelete(ctx, ref); err != nil {
				return errToolResp(err)
			}
			return fantasy.ToolResponse{Content: "deleted " + ref}, nil
		},
	)
}

// imageListInput / binListInput are empty — both tools return the
// full catalogue. Filtering is up to the model.
type imageListInput struct{}

func imageListTool(c *agentclient.Client) fantasy.AgentTool {
	desc := `List the agent images available locally. Returns ref / digest / description / size for each.

Use BEFORE agent_create to pick a base image. The "ref" column is what you pass as agent_create's ref field.

EXAMPLE OUTPUT:
  | Ref | Description | Size |
  |-----|-------------|------|
  | kubectl:latest | Kubernetes admin agent | 124MB |
  | ghcr.io/openotters/agents/web-summarizer:latest | Summarize a web page | 87MB |`
	return fantasy.NewAgentTool(
		"image_list",
		desc,
		func(ctx context.Context, _ imageListInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			images, err := c.ImageList(ctx)
			if err != nil {
				return errToolResp(err)
			}
			if len(images) == 0 {
				return fantasy.ToolResponse{
					Content: "No agent images are available locally. The operator hasn't built or pulled any yet.",
				}, nil
			}
			var b strings.Builder
			b.WriteString("| Ref | Description | Size |\n")
			b.WriteString("|-----|-------------|------|\n")
			for _, img := range images {
				fmt.Fprintf(&b, "| `%s` | %s | %d |\n", img.Ref, img.Description, img.Size)
			}
			return fantasy.ToolResponse{Content: b.String()}, nil
		},
	)
}

type selfReloadInput struct{}

func selfReloadTool(c *agentclient.Client) fantasy.AgentTool {
	desc := `Re-issue your JWT against the current link table and restart your runtime. After agent_create with self-linking, your in-memory token still carries the stale link claim — self_reload is how you pick up the refreshed set.

EXAMPLE FLOW:
  Turn N:
    agent_create({"ref":"meteo:latest","name":"Meteo","links":["<your-id>"]})
    → {"id":"...","name":"Meteo","status":"pulling"}
    self_reload({})
    → runtime restarts; this turn ENDS abruptly.
  Turn N+1 (next user message):
    agent_list → Meteo now appears.
    agent_exec({"ref":"Meteo", ...})

WARNINGS:
  • This kills the current turn — the runtime process exits mid-call. Any tool result that would have followed is lost. Call self_reload as your LAST tool.
  • Don't call self_reload more than once per turn.
  • If you're spawning a leaf worker you don't need to call yourself, skip self_reload entirely.`
	return fantasy.NewAgentTool(
		"self_reload",
		desc,
		func(ctx context.Context, _ selfReloadInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			restarted, err := c.SelfReload(ctx)
			if err != nil {
				return errToolResp(err)
			}
			if restarted {
				return fantasy.ToolResponse{
					Content: "self_reload requested; runtime is restarting. This turn ends here.",
				}, nil
			}
			return fantasy.ToolResponse{
				Content: "self_reload completed; no restart was required.",
			}, nil
		},
	)
}

func binListTool(c *agentclient.Client) fantasy.AgentTool {
	desc := `List the BIN images available locally. Returns ref / digest / description / size for each.

Use this when composing an Agentfile for agent_create_from_source — every BIN directive must reference an available BIN image. The "ref" column is what you put after BIN in your Agentfile body.

EXAMPLE — composing an Agentfile that needs curl + jq:
  1. bin_list → confirm curl:latest and jq:latest are both present.
  2. agent_create_from_source with body:
       FROM ghcr.io/openotters/runtime:latest
       MODEL anthropic/claude-sonnet-4-6
       BIN curl:latest
       BIN jq:latest`
	return fantasy.NewAgentTool(
		"bin_list",
		desc,
		func(ctx context.Context, _ imageListInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			bins, err := c.BinList(ctx)
			if err != nil {
				return errToolResp(err)
			}
			if len(bins) == 0 {
				return fantasy.ToolResponse{
					Content: "No BIN images are available locally.",
				}, nil
			}
			var b strings.Builder
			b.WriteString("| Ref | Description | Size |\n")
			b.WriteString("|-----|-------------|------|\n")
			for _, bn := range bins {
				fmt.Fprintf(&b, "| `%s` | %s | %d |\n", bn.Ref, bn.Description, bn.Size)
			}
			return fantasy.ToolResponse{Content: b.String()}, nil
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
