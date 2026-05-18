package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"go.uber.org/zap"
)

// Provider names recognised by CreateAgent. Centralised so the
// requiresAPIKey switch and the createProvider switch agree on the
// same string set, and so goconst / consumers don't have to chase
// the literal across two cases.
//
// The slugs match Catwalk's provider IDs (catwalk.charm.sh/v2/providers)
// so that operators can use the catalog's identifier directly when
// configuring providers.yaml — no aliases-table lookups, no surprises
// when the catwalk-driven model enrichment kicks in. The one exception
// is `google` (kept as an alias for `gemini`) because the fantasy
// package itself is named `google` and Vertex / AI Studio users may
// reach for that name out of muscle memory.
const (
	providerAnthropic  = "anthropic"
	providerOpenAI     = "openai"
	providerOpenRouter = "openrouter"
	providerGemini     = "gemini"
	providerGoogle     = "google"
	providerAzure      = "azure"
	providerBedrock    = "bedrock"
	providerVercel     = "vercel"
)

// Config bundles the inputs CreateAgent needs to talk to a provider.
// MaxTokens / MaxIterations always apply (we have defaults in
// agent_config.go); the sampling pointers (Temperature, TopP, TopK,
// PresencePenalty, FrequencyPenalty) are optional — only forwarded
// to fantasy when non-nil, so an unset config leaves the provider's
// own defaults in place rather than zeroing them out.
//
// ExtraAgentOpts is the escape hatch for fantasy AgentOptions the
// runtime composes outside this package — currently the notes
// PrepareStep callback. Kept as a slice (rather than a sequence of
// dedicated Config fields) so adding another optional hook doesn't
// require a new field on Config.
type Config struct {
	Provider      string
	ModelName     string
	APIKey        string
	APIBase       string
	MaxTokens     int
	MaxIterations int

	Temperature      *float64
	TopP             *float64
	TopK             *int64
	PresencePenalty  *float64
	FrequencyPenalty *float64

	ExtraAgentOpts []fantasy.AgentOption
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

	if cfg.Temperature != nil {
		opts = append(opts, fantasy.WithTemperature(*cfg.Temperature))
	}
	if cfg.TopP != nil {
		opts = append(opts, fantasy.WithTopP(*cfg.TopP))
	}
	if cfg.TopK != nil {
		opts = append(opts, fantasy.WithTopK(*cfg.TopK))
	}
	if cfg.PresencePenalty != nil {
		opts = append(opts, fantasy.WithPresencePenalty(*cfg.PresencePenalty))
	}
	if cfg.FrequencyPenalty != nil {
		opts = append(opts, fantasy.WithFrequencyPenalty(*cfg.FrequencyPenalty))
	}

	if len(tools) > 0 {
		opts = append(opts, fantasy.WithTools(tools...))
	}

	opts = append(opts, cfg.ExtraAgentOpts...)

	return fantasy.NewAgent(lm, opts...), lm, nil
}

func requiresAPIKey(provider string) bool {
	switch provider {
	case providerAnthropic,
		providerOpenAI,
		providerOpenRouter,
		providerGemini,
		providerGoogle,
		providerAzure,
		providerBedrock,
		providerVercel:
		return true
	default:
		return false
	}
}

func createProvider(name, apiKey, apiBase string) (fantasy.Provider, error) {
	switch name {
	case providerAnthropic:
		opts := []anthropic.Option{anthropic.WithAPIKey(apiKey)}
		if apiBase != "" {
			opts = append(opts, anthropic.WithBaseURL(apiBase))
		}

		return anthropic.New(opts...)
	case providerOpenAI:
		opts := []openai.Option{openai.WithAPIKey(apiKey)}
		if apiBase != "" {
			opts = append(opts, openai.WithBaseURL(apiBase))
		}

		return openai.New(opts...)
	case providerOpenRouter:
		return openrouter.New(openrouter.WithAPIKey(apiKey))
	case providerGemini, providerGoogle:
		// google's fantasy package uses WithGeminiAPIKey (NOT
		// WithAPIKey) because the same provider also handles Vertex
		// AI via WithVertex, and the package keeps the two auth
		// shapes distinct. WithBaseURL is only meaningful for the
		// Gemini direct API; Vertex sets its own URL from project +
		// location and ignores this option.
		opts := []google.Option{google.WithGeminiAPIKey(apiKey)}
		if apiBase != "" {
			opts = append(opts, google.WithBaseURL(apiBase))
		}

		return google.New(opts...)
	case providerAzure:
		opts := []azure.Option{azure.WithAPIKey(apiKey)}
		if apiBase != "" {
			opts = append(opts, azure.WithBaseURL(apiBase))
		}

		return azure.New(opts...)
	case providerBedrock:
		opts := []bedrock.Option{bedrock.WithAPIKey(apiKey)}
		if apiBase != "" {
			opts = append(opts, bedrock.WithBaseURL(apiBase))
		}

		return bedrock.New(opts...)
	case providerVercel:
		opts := []vercel.Option{vercel.WithAPIKey(apiKey)}
		if apiBase != "" {
			opts = append(opts, vercel.WithBaseURL(apiBase))
		}

		return vercel.New(opts...)
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
