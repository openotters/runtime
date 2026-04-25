package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/merlindorin/go-shared/pkg/cmd"

	"github.com/openotters/runtime/pkg/agent"
)

type Prompt struct {
	AgentConfig `embed:""`
	Message     string `arg:"" help:"Message to send to the agent"`
}

func (p *Prompt) Run(
	ctx context.Context,
	common *cmd.Commons,
	sqlite *cmd.SQLite,
) error {
	logger := common.MustLogger().Named("runtime-prompt")

	setup, err := p.setup(ctx, sqlite, logger)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "--- prompt: %s\n", p.Message)
	fmt.Fprintf(os.Stderr, "--- model: %s\n", p.Model)
	fmt.Fprintf(os.Stderr, "--- tools: %d\n", setup.toolCount)
	fmt.Fprintf(os.Stderr, "---\n\n")

	cb := func(event agent.StreamEvent) {
		switch event.Type {
		case "step.start":
			fmt.Fprintf(os.Stderr, "\n--- step %d ---\n", event.Step)
		case "step.finish":
			fmt.Fprintf(os.Stderr, "--- end step ---\n")
		case "tool.call":
			fmt.Fprintf(os.Stderr, "[tool.call] %s: %s\n", event.ToolName, truncate(event.Content, 200))
		case "tool.result":
			fmt.Fprintf(os.Stderr, "[tool.result] %s: %s\n", event.ToolName, truncate(event.Content, 200))
		case "text.delta":
			os.Stdout.WriteString(event.Content)
		}
	}

	_, err = setup.svc.ChatStream(ctx, "prompt-debug", p.Message, cb)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}

	os.Stdout.WriteString("\n")

	return nil
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}

	return s
}
