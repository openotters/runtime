package commands

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"github.com/merlindorin/go-shared/pkg/cmd"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/openotters/runtime/pkg/agent"
	"github.com/openotters/runtime/pkg/memory"
	"github.com/openotters/runtime/pkg/notes"
	"github.com/openotters/runtime/pkg/tool"
)

// ToolConfig mirrors a tools[] entry in the agent.yaml shape produced
// by the daemon's materialise. Usage is a path (absolute from agent
// root) to the USAGE.md the loader reads when assembling the
// model-facing tool description.
type ToolConfig struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	Binary      string   `json:"binary" yaml:"binary"`
	Args        []string `json:"args,omitempty" yaml:"args,omitempty"`
	Usage       string   `json:"usage,omitempty" yaml:"usage,omitempty"`
}

// MemoryServeConfig holds the compactor's tunables. Populated from
// agent.yaml's configs: block via loadAgentConfig — kebab-case keys
// (`memory-strategy`, `memory-max-messages`, `memory-max-tokens`)
// match the Agentfile's CONFIG directive shape 1:1.
type MemoryServeConfig struct {
	Strategy    string `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	MaxMessages int    `json:"max_messages,omitempty" yaml:"max_messages,omitempty"`
	MaxTokens   int    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

// NotesServeConfig holds the notes-tool quotas + the prompt-section
// placement switch. Like MemoryServeConfig, populated from
// agent.yaml's configs: block — keys `notes-max-bytes-per`,
// `notes-max-count`, `notes-prompt-section`.
type NotesServeConfig struct {
	MaxBytesPer   int    `json:"max_bytes_per,omitempty" yaml:"max_bytes_per,omitempty"`
	MaxCount      int    `json:"max_count,omitempty"      yaml:"max_count,omitempty"`
	PromptSection string `json:"prompt_section,omitempty" yaml:"prompt_section,omitempty"`
}

// Runtime tunables come from agent.yaml's configs: block, not CLI
// flags. The Agentfile declares them (CONFIG max-tokens 2048, …),
// the daemon stamps the resolved map into agent.yaml, and the
// runtime parses them into typed fields below. Defaults live in
// applyConfigDefaults; configs entries override.
const (
	defaultMaxTokens          = 4096
	defaultMaxIterations      = 20
	defaultMemoryStrategy     = "summarize"
	defaultMemoryMaxMessages  = 20
	defaultMemoryMaxTokens    = 0
	defaultNotesMaxBytesPer   = 4096
	defaultNotesMaxCount      = 64
	defaultNotesPromptSection = "off"
)

type AgentConfig struct {
	Root string `help:"Agent root directory (FHS layout)" default:"/"`

	// Provider/model wiring is operator-controlled, kept on the CLI
	// surface so dev invocations of the runtime binary outside the
	// daemon can still run a one-shot chat. Inside the daemon
	// they're set from agent.yaml + the provider catalog.
	Model   string `help:"Model identifier (e.g. anthropic/claude-sonnet-4-20250514)" optional:""`
	Name    string `help:"Agent name" default:"agent"`
	APIKey  string `help:"API key for the provider" optional:""`
	APIBase string `help:"Custom API base URL for the provider" optional:""`
	Addr    string `help:"gRPC listen address" default:":8080"`

	// Runtime tunables — never CLI flags. Populated from agent.yaml's
	// `configs:` block by loadAgentConfig. Plain Go fields with
	// `kong:"-"` so Kong leaves them untouched.
	MaxTokens     int               `kong:"-"`
	MaxIterations int               `kong:"-"`
	Memory        MemoryServeConfig `kong:"-"`
	Notes         NotesServeConfig  `kong:"-"`

	// Provider sampling knobs. Pointer types so "unset" stays
	// distinguishable from a deliberately-zero value (temperature: 0
	// means deterministic, not "use the default"). Forwarded to
	// fantasy only when non-nil.
	Temperature      *float64 `kong:"-"`
	TopP             *float64 `kong:"-"`
	TopK             *int64   `kong:"-"`
	PresencePenalty  *float64 `kong:"-"`
	FrequencyPenalty *float64 `kong:"-"`

	// Persisted-config inputs from agent.yaml: tools list and the
	// ordered context files. Also never CLI flags.
	Tools   []ToolConfig        `kong:"-" yaml:"tools,omitempty" json:"tools,omitempty"`
	Context []agent.ContextFile `kong:"-" yaml:"context,omitempty" json:"context,omitempty"`
}

func (c *AgentConfig) contextDir() string   { return filepath.Join(c.Root, "etc", "context") }
func (c *AgentConfig) dataDir() string      { return filepath.Join(c.Root, "etc", "data") }
func (c *AgentConfig) binDir() string       { return filepath.Join(c.Root, "usr", "bin") }
func (c *AgentConfig) workspaceDir() string { return filepath.Join(c.Root, "workspace") }
func (c *AgentConfig) tmpDir() string       { return filepath.Join(c.Root, "tmp") }
func (c *AgentConfig) dbPath() string       { return filepath.Join(c.Root, "var", "lib", "memory.db") }

type agentSetup struct {
	svc          *agent.Service
	systemPrompt string
	toolCount    int
}

func (c *AgentConfig) setup(
	ctx context.Context, sqlite *cmd.SQLite, logger *zap.Logger,
) (*agentSetup, error) {
	c.loadAgentConfig(logger)

	if c.Model == "" {
		return nil, fmt.Errorf(
			"model is required: use --model provider/model (e.g. anthropic/claude-sonnet-4-20250514)",
		)
	}

	if err := c.ensureDirs(); err != nil {
		return nil, err
	}

	// Context files come from agent.yaml's `context:` list. The
	// runtime no longer scans etc/context/ — the daemon declares
	// exactly which files load and in what order. Each entry carries
	// its display name (used as the section header in the system
	// prompt) and an absolute path resolved against c.Root.
	systemPrompt, err := agent.BuildSystemPrompt(c.Root, c.Context)
	if err != nil {
		return nil, err
	}

	// Surface the assembled prompt size so a silent context-load
	// regression (missing files, wrong root, empty agent.yaml) is
	// visible at startup instead of only via "the agent feels off".
	logger.Info("system prompt built",
		zap.Int("files", len(c.Context)),
		zap.Int("bytes", len(systemPrompt)),
	)

	// Open the agent's sqlite database once and hand the same
	// connection to both stores (messages + notes). Two stores on
	// one *sql.DB is fine — SQLite handles concurrent connections
	// to one file via WAL, but a single Go connection avoids the
	// locking entirely.
	db, err := c.openDB(sqlite)
	if err != nil {
		return nil, err
	}
	memStore, err := memory.NewStore(ctx, db)
	if err != nil {
		return nil, err
	}
	notesStore, err := notes.NewStore(ctx, db)
	if err != nil {
		return nil, err
	}

	tools, err := c.loadTools(notesStore, logger)
	if err != nil {
		return nil, err
	}

	provider, modelName := parseModel(c.Model)

	// validateModel was an OpenAI-compatible /models/<name> probe; it 404s
	// on Anthropic and would 401 on most non-OpenAI providers without
	// per-flavour adapters. Real model errors surface on the first
	// fantasy.Generate call anyway, so we skip the early probe.

	// Notes PrepareStep is always installed: it surfaces pinned
	// (in_context) notes into the system prompt on every step,
	// regardless of the notes-prompt-section setting. The section
	// value only controls the OPTIONAL preview-table of all notes —
	// "off" (default) leaves the model tools-only for non-pinned
	// notes; "above" / "below" adds the table at that placement.
	extraOpts := []fantasy.AgentOption{
		fantasy.WithPrepareStep(
			agent.BuildNotesPrepareStep(systemPrompt, notesStore, c.Notes.PromptSection, logger),
		),
	}
	logger.Info("notes prepare-step installed",
		zap.String("section", c.Notes.PromptSection))

	fantasyAgent, lm, err := agent.CreateAgent(ctx, agent.Config{
		Provider: provider, ModelName: modelName,
		APIKey: c.APIKey, APIBase: c.APIBase,
		MaxTokens: c.MaxTokens, MaxIterations: c.MaxIterations,
		Temperature: c.Temperature, TopP: c.TopP, TopK: c.TopK,
		PresencePenalty: c.PresencePenalty, FrequencyPenalty: c.FrequencyPenalty,
		ExtraAgentOpts: extraOpts,
	}, systemPrompt, tools, logger)
	if err != nil {
		return nil, err
	}

	logger.Info("model validated", zap.String("model", c.Model))

	compactor := memory.NewCompactor(memory.Config{
		Strategy:    c.Memory.Strategy,
		MaxMessages: c.Memory.MaxMessages,
		MaxTokens:   c.Memory.MaxTokens,
	}, logger)

	return &agentSetup{
		svc:          agent.NewService(fantasyAgent, lm, memStore, compactor, logger),
		systemPrompt: systemPrompt,
		toolCount:    len(tools),
	}, nil
}

func (c *AgentConfig) ensureDirs() error {
	dirs := []string{c.contextDir(), c.dataDir(), c.binDir(), c.workspaceDir(), c.tmpDir()}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	dbDir := filepath.Dir(c.dbPath())

	return os.MkdirAll(dbDir, 0o755)
}

// openDB returns the agent's sqlite *sql.DB handle, defaulting the
// sqlite Path to dbPath() when the operator left it as the in-memory
// sentinel. Split out from openStore so memory.Store and notes.Store
// can share the same connection.
func (c *AgentConfig) openDB(sqlite *cmd.SQLite) (*sql.DB, error) {
	if sqlite.Path == ":memory:" {
		sqlite.Path = c.dbPath()
	}
	return sqlite.Open()
}

func (c *AgentConfig) loadTools(notesStore *notes.Store, logger *zap.Logger) ([]fantasy.AgentTool, error) {
	defs := make([]tool.Def, len(c.Tools))
	for i, t := range c.Tools {
		binary := t.Binary
		if !filepath.IsAbs(binary) {
			binary = filepath.Join(c.binDir(), t.Name)
		}

		defs[i] = tool.Def{
			Name: t.Name, Description: t.Description,
			Binary: binary, Args: t.Args,
			Docs: tool.Docs{Usage: c.resolveDocPath(t.Usage)},
		}
	}

	return tool.LoadTools(defs, c.Root, notesStore, c.Notes.MaxBytesPer, c.Notes.MaxCount, logger)
}

// resolveDocPath rewrites a doc path the executor stamped into
// agent.yaml so the loader can open it. Relative paths join against
// the agent root (system executor's convention — same shape as
// Binary); absolute paths are used verbatim (docker executor's
// convention for image-mount paths). Empty in → empty out so the
// loader skips silently.
func (c *AgentConfig) resolveDocPath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(c.Root, path)
}

func parseModel(model string) (string, string) {
	if idx := strings.Index(model, "/"); idx > 0 {
		return model[:idx], model[idx+1:]
	}

	return "", model
}

type agentYAML struct {
	Name    string            `yaml:"name"`
	Model   string            `yaml:"model"`
	Configs map[string]string `yaml:"configs,omitempty"`
	// Context is the daemon-supplied list of context files to load,
	// each with a short name (for context_show), an absolute path,
	// and a one-line description. The runtime reads File to load
	// the markdown into the system prompt; the introspect tools
	// surface Name + Description.
	Context []struct {
		Name        string `yaml:"name"`
		File        string `yaml:"file"`
		Description string `yaml:"description,omitempty"`
	} `yaml:"context,omitempty"`
	Tools []struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Binary      string `yaml:"binary"`
		Usage       string `yaml:"usage,omitempty"`
	} `yaml:"tools,omitempty"`
}

// loadAgentConfig reads etc/agent.yaml from the root directory.
// Configs, tools, and context are sourced from this file; the
// remaining CLI-visible fields (Name, Model, APIBase, APIKey, Addr)
// are filled when the value the operator passed at spawn time is
// empty. Tunables (max-tokens, memory-*, …) are always overwritten
// from configs: — there is no CLI for them.
func (c *AgentConfig) loadAgentConfig(logger *zap.Logger) {
	c.applyConfigDefaults()

	path := filepath.Join(c.Root, "etc", "agent.yaml")

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var cfg agentYAML
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		logger.Warn("failed to parse agent config", zap.String("path", path), zap.Error(err))
		return
	}

	if c.Name == "agent" && cfg.Name != "" {
		c.Name = cfg.Name
	}

	if c.Model == "" && cfg.Model != "" {
		c.Model = cfg.Model
	}

	c.applyConfigsMap(cfg.Configs, logger)

	if len(c.Tools) == 0 && len(cfg.Tools) > 0 {
		for _, t := range cfg.Tools {
			binary := t.Binary
			if !filepath.IsAbs(binary) {
				binary = filepath.Join(c.Root, binary)
			}

			c.Tools = append(c.Tools, ToolConfig{
				Name:        t.Name,
				Description: t.Description,
				Binary:      binary,
				Usage:       t.Usage,
			})
		}
	}

	if len(c.Context) == 0 && len(cfg.Context) > 0 {
		for _, e := range cfg.Context {
			c.Context = append(c.Context, agent.ContextFile{Name: e.Name, Path: e.File})
		}
	}

	logger.Info("loaded agent config", zap.String("path", path))
}

// applyConfigDefaults seeds the tunable fields. Called before
// applyConfigsMap so any entry in agent.yaml's configs: block wins.
func (c *AgentConfig) applyConfigDefaults() {
	c.MaxTokens = defaultMaxTokens
	c.MaxIterations = defaultMaxIterations
	c.Memory.Strategy = defaultMemoryStrategy
	c.Memory.MaxMessages = defaultMemoryMaxMessages
	c.Memory.MaxTokens = defaultMemoryMaxTokens
	c.Notes.MaxBytesPer = defaultNotesMaxBytesPer
	c.Notes.MaxCount = defaultNotesMaxCount
	c.Notes.PromptSection = defaultNotesPromptSection
}

// applyConfigsMap reads the kebab-case keys the Agentfile's CONFIG
// directives produce (and that the daemon stamps into agent.yaml).
// Unknown keys are logged at debug level so a typo in the Agentfile
// surfaces but doesn't crash the runtime.
//
// Recognised keys:
//   - `max-tokens` (int)        — provider max output tokens per call.
//   - `max-iterations` (int)    — fantasy step-count stop condition.
//   - `memory-strategy` (str)   — `summarize` | `truncate`.
//   - `memory-max-messages` (int) — compactor message-count budget.
//   - `memory-max-tokens` (int) — compactor token budget (0 = disabled).
//   - `temperature` (float)     — sampling temperature; 0 = deterministic.
//   - `top-p` (float)           — nucleus-sampling cutoff.
//   - `top-k` (int)             — top-K sampling cutoff.
//   - `presence-penalty` (float)  — discourage repeating tokens.
//   - `frequency-penalty` (float) — discourage frequent tokens.
//   - `notes-max-bytes-per` (int) — per-note size cap; note_save rejects past this.
//   - `notes-max-count` (int)     — total notes cap; note_save rejects new keys past this.
//   - `notes-prompt-section` (str) — `off` (default) / `above` / `below`. When set,
//     a PrepareStep callback injects a `## Notes` table into the system prompt
//     on every step.
//
// Sampling keys are forwarded to fantasy only when present in the map
// so an unspecified knob leaves the provider's own default alone.
//
// branch is trivial. Splitting into per-family helpers would just push
// the chain up one level without changing how anyone reads it.
//
//nolint:gocognit // a flat dispatch of kebab-key → struct field; each
func (c *AgentConfig) applyConfigsMap(configs map[string]string, logger *zap.Logger) {
	for k, v := range configs {
		switch k {
		case "max-tokens":
			if n, err := strconv.Atoi(v); err == nil {
				c.MaxTokens = n
			}
		case "max-iterations":
			if n, err := strconv.Atoi(v); err == nil {
				c.MaxIterations = n
			}
		case "memory-strategy":
			if v != "" {
				c.Memory.Strategy = v
			}
		case "memory-max-messages":
			if n, err := strconv.Atoi(v); err == nil {
				c.Memory.MaxMessages = n
			}
		case "memory-max-tokens":
			if n, err := strconv.Atoi(v); err == nil {
				c.Memory.MaxTokens = n
			}
		case "temperature":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				c.Temperature = &f
			}
		case "top-p":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				c.TopP = &f
			}
		case "top-k":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				c.TopK = &n
			}
		case "presence-penalty":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				c.PresencePenalty = &f
			}
		case "frequency-penalty":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				c.FrequencyPenalty = &f
			}
		case "notes-max-bytes-per":
			if n, err := strconv.Atoi(v); err == nil {
				c.Notes.MaxBytesPer = n
			}
		case "notes-max-count":
			if n, err := strconv.Atoi(v); err == nil {
				c.Notes.MaxCount = n
			}
		case "notes-prompt-section":
			switch v {
			case "off", "above", "below":
				c.Notes.PromptSection = v
			default:
				logger.Warn("invalid notes-prompt-section value; keeping default",
					zap.String("value", v),
					zap.String("default", c.Notes.PromptSection))
			}
		default:
			logger.Debug("unknown config key", zap.String("key", k), zap.String("value", v))
		}
	}
}
