package neighbor

import (
	"context"
	"fmt"

	"charm.land/fantasy"
	"go.uber.org/zap"
)

type MessageInput struct {
	Message string `json:"message" jsonschema:"description=The message to send to the neighbor agent"`
}

func BuildNeighborTools(neighbors []Config, logger *zap.Logger) []fantasy.AgentTool {
	tools := make([]fantasy.AgentTool, 0, len(neighbors))

	for _, cfg := range neighbors {
		client := NewClient(cfg)

		t := fantasy.NewAgentTool(
			"message_"+cfg.Name,
			fmt.Sprintf("Send a message to agent '%s' and get their response.", cfg.Name),
			func(ctx context.Context, input MessageInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				resp, err := client.SendMessage(ctx, input.Message)
				if err != nil {
					return fantasy.ToolResponse{
						IsError: true,
						Content: fmt.Sprintf("failed to reach %s: %s", cfg.Name, err),
					}, nil
				}

				return fantasy.ToolResponse{Content: resp}, nil
			},
		)

		tools = append(tools, t)

		logger.Info("neighbor tool registered", zap.String("agent", cfg.Name), zap.String("url", cfg.URL))
	}

	return tools
}
