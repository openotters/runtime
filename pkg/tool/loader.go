package tool

import (
	"os"
	"strings"

	"charm.land/fantasy"
	"go.uber.org/zap"

	"github.com/openotters/runtime/pkg/notesclient"
)

// Def declares a tool the runtime should register with the LLM. The
// shape is intentionally split between "what the tool does" (Name,
// Description, Binary, Args) and "what extra reading material the
// model gets" (Docs). The agentfile executor populates Docs from
// the BIN image's `vnd.openotters.bin.*` annotations.
type Def struct {
	Name        string
	Description string
	Binary      string
	Args        []string
	Docs        Docs
}

// Docs are documentation artefacts the executor materialised next to
// the binary. Each field is a path on the runtime's local
// filesystem; empty / missing files are tolerated silently so a BIN
// without docs degrades to "tool with short description only."
//
// New annotation-driven artefacts (examples, schema, FAQ, …) plug
// in here as additional fields without breaking the runtime's
// agent.yaml schema.
type Docs struct {
	// Usage is a USAGE.md-style long-form description. Appended to
	// the model-facing tool description under a "## Usage" section
	// at registration time.
	Usage string
}

// LoadTools assembles the runtime's full tool set: the BIN-backed
// tools declared in agent.yaml, the daemon-callback job tools
// (when the agent has a daemon URL + token), the introspection
// tools (context_*, env_list, mount_list), and — when notesStore
// is non-nil — the note_* tools.
//
// caps is the agent's runtime cap allowlist (from agent.yaml's
// capabilities: block, which mirrors the daemon's
// JWT.Capabilities claim). Auto-injected tools (job_*, agent_*,
// note_*, introspection) are filtered against this list — a tool
// whose name isn't in caps is NOT registered. The model literally
// can't see tools it can't call. Defence in depth alongside the
// daemon's requireCapability RPC gate.
//
// BIN-declared tools (defs) are always registered — the
// CAPABILITY directive doesn't gate per-BIN tools; an Agentfile's
// BIN directive is itself the grant for that BIN.
//
// notesClient is intentionally optional: dev invocations of the
// runtime binary without a daemon (e.g. a one-shot CLI prompt)
// still work, just without the notes capability registered.
func LoadTools(
	defs []Def, workDir string, caps []string,
	notesClient notesclient.Store, notesMaxBytes, notesMaxCount int,
	logger *zap.Logger,
) ([]fantasy.AgentTool, error) {
	tools := make([]fantasy.AgentTool, 0, len(defs))

	for _, cfg := range defs {
		description := composeDescription(cfg.Description, cfg.Docs)

		executor := newExecutor(cfg.Binary, cfg.Args, workDir, logger)

		t := fantasy.NewAgentTool(
			cfg.Name,
			description,
			executor.Run,
		)

		tools = append(tools, t)

		logger.Info("tool loaded", zap.String("name", cfg.Name), zap.String("binary", cfg.Binary))
	}

	hasCap := makeCapSet(caps)

	// Each auto-injected tool registers only if its name is in
	// caps. filterTools is a small helper that drops un-granted
	// names from the slice; tools registered by the builders
	// already carry their canonical name.
	if jobTools := filterTools(BuildJobTools(), hasCap); len(jobTools) > 0 {
		tools = append(tools, jobTools...)
		logger.Info("daemon-job tools registered", zap.Int("count", len(jobTools)))
	}

	if agentTools := filterTools(BuildAgentTools(), hasCap); len(agentTools) > 0 {
		tools = append(tools, agentTools...)
		logger.Info("agent-linking tools registered", zap.Int("count", len(agentTools)))
	}

	if introspect := filterTools(BuildIntrospectionTools(workDir), hasCap); len(introspect) > 0 {
		tools = append(tools, introspect...)
		logger.Info("introspection tools registered", zap.Int("count", len(introspect)))
	}

	// Interface nil-check: agent_config.go passes nil when no
	// daemon callback is configured. Bare `!= nil` would be true
	// for an interface holding a typed nil; guard the concrete
	// client pointer explicitly via the parameter type.
	if notesClient != nil {
		if ns := filterTools(BuildNotesTools(notesClient, notesMaxBytes, notesMaxCount), hasCap); len(ns) > 0 {
			tools = append(tools, ns...)
			logger.Info("notes tools registered", zap.Int("count", len(ns)))
		}
	}

	return tools, nil
}

func makeCapSet(caps []string) map[string]struct{} {
	out := make(map[string]struct{}, len(caps))
	for _, c := range caps {
		out[c] = struct{}{}
	}
	return out
}

// filterTools drops any tool whose name isn't in the cap allowlist.
// Used by LoadTools to gate every auto-injected tool on the
// agent.yaml capabilities: list.
func filterTools(tools []fantasy.AgentTool, hasCap map[string]struct{}) []fantasy.AgentTool {
	if len(tools) == 0 {
		return tools
	}
	out := make([]fantasy.AgentTool, 0, len(tools))
	for _, t := range tools {
		if _, ok := hasCap[t.Info().Name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// composeDescription stitches the short BIN directive description
// together with any doc artefacts present on disk. Missing files
// are tolerated: the loader degrades to "description only" rather
// than failing the agent start.
func composeDescription(short string, docs Docs) string {
	var b strings.Builder
	b.WriteString(short)

	if body := readDocFile(docs.Usage); body != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}

		b.WriteString("## Usage\n\n")
		b.WriteString(body)
	}

	return b.String()
}

func readDocFile(path string) string {
	if path == "" {
		return ""
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}
