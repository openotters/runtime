package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"go.uber.org/zap"
)

type Config struct {
	Provider      string
	ModelName     string
	APIKey        string
	APIBase       string
	MaxTokens     int
	MaxIterations int
}

func CreateAgent(
	ctx context.Context, cfg Config, systemPrompt string,
	tools []fantasy.AgentTool, logger *zap.Logger,
) (fantasy.Agent, fantasy.LanguageModel, error) {
	if cfg.Provider == "" {
		return nil, nil, fmt.Errorf(
			"invalid model format: expected 'provider/model' (e.g. anthropic/claude-sonnet-4-20250514), got %q",
			cfg.ModelName,
		)
	}

	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = os.Getenv(strings.ToUpper(cfg.Provider) + "_API_KEY")
	}

	if apiKey == "" && requiresAPIKey(cfg.Provider) {
		return nil, nil, fmt.Errorf("no API key for provider %s", cfg.Provider)
	}

	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = os.Getenv(strings.ToUpper(cfg.Provider) + "_API_BASE")
	}

	provider, err := createProvider(cfg.Provider, apiKey, apiBase)
	if err != nil {
		return nil, nil, fmt.Errorf("creating provider: %w", err)
	}

	lm, err := provider.LanguageModel(ctx, cfg.ModelName)
	if err != nil {
		return nil, nil, fmt.Errorf("getting language model: %w", err)
	}

	logger.Info("agent created",
		zap.String("provider", cfg.Provider),
		zap.String("model", cfg.ModelName),
		zap.Int("tools", len(tools)),
	)

	opts := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithMaxOutputTokens(int64(cfg.MaxTokens)),
		fantasy.WithStopConditions(fantasy.StepCountIs(cfg.MaxIterations)),
	}

	if len(tools) > 0 {
		opts = append(opts, fantasy.WithTools(tools...))
	}

	return fantasy.NewAgent(lm, opts...), lm, nil
}

func requiresAPIKey(provider string) bool {
	switch provider {
	case "anthropic", "openai", "openrouter":
		return true
	default:
		return false
	}
}

func createProvider(name, apiKey, apiBase string) (fantasy.Provider, error) {
	switch name {
	case "anthropic":
		opts := []anthropic.Option{anthropic.WithAPIKey(apiKey)}
		if apiBase != "" {
			opts = append(opts, anthropic.WithBaseURL(apiBase))
		}

		return anthropic.New(opts...)
	case "openai":
		opts := []openai.Option{openai.WithAPIKey(apiKey)}
		if apiBase != "" {
			opts = append(opts, openai.WithBaseURL(apiBase))
		}

		return openai.New(opts...)
	case "openrouter":
		return openrouter.New(openrouter.WithAPIKey(apiKey))
	default:
		opts := []openaicompat.Option{
			openaicompat.WithAPIKey(apiKey),
			openaicompat.WithName(name),
		}
		if apiBase != "" {
			opts = append(opts, openaicompat.WithBaseURL(apiBase))
		}

		return openaicompat.New(opts...)
	}
}
