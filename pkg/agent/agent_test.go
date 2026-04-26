//nolint:testpackage // tests the unexported requiresAPIKey predicate
package agent

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestRequiresAPIKey(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"anthropic":  true,
		"openai":     true,
		"openrouter": true,
		"ollama":     false,
		"custom":     false,
		"":           false,
	}

	for provider, want := range cases {
		if got := requiresAPIKey(provider); got != want {
			t.Errorf("requiresAPIKey(%q) = %v, want %v", provider, got, want)
		}
	}
}

func TestCreateAgent_EmptyProviderErrors(t *testing.T) {
	t.Parallel()

	_, _, err := CreateAgent(
		context.Background(),
		Config{Provider: "", ModelName: "claude"},
		"sysprompt", nil, zap.NewNop(),
	)
	if err == nil {
		t.Fatal("expected error for empty provider, got nil")
	}

	if !strings.Contains(err.Error(), "invalid model format") {
		t.Errorf("expected 'invalid model format' error, got %v", err)
	}
}

func TestCreateAgent_MissingAPIKeyErrors(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, _, err := CreateAgent(
		context.Background(),
		Config{Provider: "anthropic", ModelName: "claude-sonnet-4"},
		"sys", nil, zap.NewNop(),
	)
	if err == nil {
		t.Fatal("expected error for missing API key, got nil")
	}

	if !strings.Contains(err.Error(), "no API key") {
		t.Errorf("expected 'no API key' error, got %v", err)
	}
}

func TestCreateAgent_PicksUpAPIKeyFromEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-fake")

	_, _, err := CreateAgent(
		context.Background(),
		Config{
			Provider:      "anthropic",
			ModelName:     "claude-sonnet-4",
			MaxTokens:     1024,
			MaxIterations: 4,
		},
		"sys", nil, zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("CreateAgent with env API key: %v", err)
	}
}

func TestCreateAgent_PicksUpAPIBaseFromEnv(t *testing.T) {
	// Mirrors the existing _API_KEY env-fallback test: when
	// cfg.APIBase is empty, we fall through to <PROVIDER>_API_BASE.
	// Asserted via the no-error path on a provider (ollama →
	// openaicompat) that requires no API key, so the test isolates
	// the api-base resolution from the api-key resolution.
	t.Setenv("OLLAMA_API_BASE", "http://localhost:11434/v1")

	_, _, err := CreateAgent(
		context.Background(),
		Config{
			Provider:      "ollama",
			ModelName:     "llama3.1:8b",
			MaxTokens:     1024,
			MaxIterations: 2,
		},
		"sys", nil, zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("CreateAgent with env API base: %v", err)
	}
}

func TestCreateAgent_OllamaProviderWithoutAPIKey(t *testing.T) {
	t.Parallel()

	// Non-anthropic/openai/openrouter providers go through openaicompat,
	// which doesn't require a key — covers the default branch in
	// createProvider + the requiresAPIKey=false path.
	_, _, err := CreateAgent(
		context.Background(),
		Config{
			Provider:      "ollama",
			ModelName:     "llama3.1:8b",
			APIBase:       "http://localhost:11434/v1",
			MaxTokens:     1024,
			MaxIterations: 2,
		},
		"sys", nil, zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("CreateAgent (ollama): %v", err)
	}
}
