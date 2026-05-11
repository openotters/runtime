package tool

import (
	"os"
	"strings"

	"charm.land/fantasy"
	"go.uber.org/zap"
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

func LoadTools(defs []Def, workDir string, logger *zap.Logger) ([]fantasy.AgentTool, error) {
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

	// Daemon-callback tools (job_submit / job_status / job_wait).
	// Auto-registered when both OTTERSD_URL and OTTERS_AGENT_TOKEN
	// are present in the spawn env — the agentfile executor plants
	// them at CreateAgent time. Absence means an old daemon
	// (pre-JWT) or --no-http; the agent operates with only the
	// per-BIN sync exec tools above, no error.
	if jobTools := BuildJobTools(); len(jobTools) > 0 {
		tools = append(tools, jobTools...)
		logger.Info("daemon-job tools registered", zap.Int("count", len(jobTools)))
	}

	return tools, nil
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
