package tool

import (
	"os"
	"strings"

	"charm.land/fantasy"
	"go.uber.org/zap"
)

type Def struct {
	Name        string
	Description string
	Binary      string
	Args        []string
	Doc         string
}

func LoadTools(defs []Def, workDir string, logger *zap.Logger) ([]fantasy.AgentTool, error) {
	tools := make([]fantasy.AgentTool, 0, len(defs))

	for _, cfg := range defs {
		description := cfg.Description

		if cfg.Doc != "" {
			if data, err := os.ReadFile(cfg.Doc); err == nil {
				description += "\n\n" + strings.TrimSpace(string(data))
			}
		}

		executor := newExecutor(cfg.Binary, cfg.Args, workDir, logger)

		t := fantasy.NewAgentTool(
			cfg.Name,
			description,
			executor.Run,
		)

		tools = append(tools, t)

		logger.Info("tool loaded", zap.String("name", cfg.Name), zap.String("binary", cfg.Binary))
	}

	return tools, nil
}
