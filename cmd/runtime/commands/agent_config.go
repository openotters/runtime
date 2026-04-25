package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/merlindorin/go-shared/pkg/cmd"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/openotters/runtime/pkg/agent"
	"github.com/openotters/runtime/pkg/memory"
	"github.com/openotters/runtime/pkg/neighbor"
	"github.com/openotters/runtime/pkg/tool"
)

type ToolConfig struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	Binary      string   `json:"binary" yaml:"binary"`
	Args        []string `json:"args,omitempty" yaml:"args,omitempty"`
}

type NeighborConfig struct {
	Name  string `json:"name" yaml:"name"`
	URL   string `json:"url" yaml:"url"`
	WSUrl string `json:"ws_url,omitempty" yaml:"ws_url,omitempty"`
	Token string `json:"token,omitempty" yaml:"token,omitempty"`
}

type MemoryServeConfig struct {
	Strategy    string `json:"strategy,omitempty" yaml:"strategy,omitempty" help:"Compaction strategy (sliding or summarize)" default:"summarize"`
	MaxMessages int    `json:"max_messages,omitempty" yaml:"max_messages,omitempty" help:"Max messages before compaction" default:"20"`
	MaxTokens   int    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty" help:"Max estimated tokens before compaction" default:"0"`
}

type AgentConfig struct {
	Root string `help:"Agent root directory (FHS layout)" default:"."`

	Model         string `help:"Model identifier (e.g. anthropic/claude-sonnet-4-20250514)" optional:""`
	Name          string `help:"Agent name" default:"agent"`
	MaxTokens     int    `help:"Max output tokens" default:"4096"`
	MaxIterations int    `help:"Max tool iterations per turn" default:"20"`

	APIKey  string `help:"API key for the provider" optional:""`
	APIBase string `help:"Custom API base URL for the provider" optional:""`
	Addr    string `help:"gRPC listen address" default:":8080"`

	Tools     []ToolConfig      `help:"Tool configurations" yaml:"tools,omitempty" json:"tools,omitempty"`
	Neighbors []NeighborConfig  `help:"Neighbor agent configurations" yaml:"neighbors,omitempty" json:"neighbors,omitempty"`
	Memory    MemoryServeConfig `embed:"" prefix:"memory-"`
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

	contextFiles, err := c.discoverContextFiles()
	if err != nil {
		return nil, err
	}

	systemPrompt, err := agent.BuildSystemPrompt(c.contextDir(), contextFiles)
	if err != nil {
		return nil, err
	}

	tools, err := c.loadTools(logger)
	if err != nil {
		return nil, err
	}

	provider, modelName := parseModel(c.Model)

	// validateModel was an OpenAI-compatible /models/<name> probe; it 404s
	// on Anthropic and would 401 on most non-OpenAI providers without
	// per-flavour adapters. Real model errors surface on the first
	// fantasy.Generate call anyway, so we skip the early probe.

	fantasyAgent, lm, err := agent.CreateAgent(ctx, agent.Config{
		Provider: provider, ModelName: modelName,
		APIKey: c.APIKey, APIBase: c.APIBase,
		MaxTokens: c.MaxTokens, MaxIterations: c.MaxIterations,
	}, systemPrompt, tools, logger)
	if err != nil {
		return nil, err
	}

	logger.Info("model validated", zap.String("model", c.Model))

	store, err := c.openStore(ctx, sqlite)
	if err != nil {
		return nil, err
	}

	compactor := memory.NewCompactor(memory.Config{
		Strategy:    c.Memory.Strategy,
		MaxMessages: c.Memory.MaxMessages,
		MaxTokens:   c.Memory.MaxTokens,
	}, logger)

	return &agentSetup{
		svc:          agent.NewService(fantasyAgent, lm, store, compactor, logger),
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

func (c *AgentConfig) discoverContextFiles() ([]string, error) {
	entries, err := os.ReadDir(c.contextDir())
	if err != nil {
		return nil, fmt.Errorf("reading context dir: %w", err)
	}

	var files []string

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		if strings.HasSuffix(e.Name(), ".md") {
			files = append(files, e.Name())
		}
	}

	return files, nil
}

func (c *AgentConfig) openStore(ctx context.Context, sqlite *cmd.SQLite) (*memory.Store, error) {
	if sqlite.Path == ":memory:" {
		sqlite.Path = c.dbPath()
	}

	db, err := sqlite.Open()
	if err != nil {
		return nil, err
	}

	return memory.NewStore(ctx, db)
}

func (c *AgentConfig) loadTools(logger *zap.Logger) ([]fantasy.AgentTool, error) {
	defs := make([]tool.Def, len(c.Tools))
	for i, t := range c.Tools {
		binary := t.Binary
		if !filepath.IsAbs(binary) {
			binary = filepath.Join(c.binDir(), t.Name)
		}

		defs[i] = tool.Def{
			Name: t.Name, Description: t.Description,
			Binary: binary, Args: t.Args,
		}
	}

	tools, err := tool.LoadTools(defs, c.Root, logger)
	if err != nil {
		return nil, err
	}

	neighborCfgs := make([]neighbor.Config, len(c.Neighbors))
	for i, n := range c.Neighbors {
		neighborCfgs[i] = neighbor.Config{Name: n.Name, URL: n.URL, Token: n.Token}
	}

	tools = append(tools, neighbor.BuildNeighborTools(neighborCfgs, logger)...)

	return tools, nil
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
	Tools   []struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Binary      string `yaml:"binary"`
	} `yaml:"tools,omitempty"`
}

// loadAgentConfig reads etc/agent.yaml from the root directory and applies
// values as defaults — CLI flags and env vars still take precedence.
func (c *AgentConfig) loadAgentConfig(logger *zap.Logger) {
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
			})
		}
	}

	logger.Info("loaded agent config", zap.String("path", path))
}
