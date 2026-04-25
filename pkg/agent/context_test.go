package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openotters/runtime/pkg/agent"
)

func TestBuildSystemPrompt_ConcatenatesPresentFilesWithSeparator(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("rules\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "TOOLS.md"), []byte("\n  tools\n  "), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := agent.BuildSystemPrompt(dir, []string{"AGENTS.md", "MISSING.md", "TOOLS.md"})
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if !strings.Contains(got, "## AGENTS.md") || !strings.Contains(got, "## TOOLS.md") {
		t.Fatalf("missing per-file headers in prompt:\n%s", got)
	}

	if !strings.Contains(got, "\n\n---\n\n") {
		t.Fatalf("missing separator between sections:\n%s", got)
	}

	if strings.Contains(got, "MISSING.md") {
		t.Fatalf("non-existent file leaked into prompt:\n%s", got)
	}
}

func TestBuildSystemPrompt_EmptyFilesAreSkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "EMPTY.md"), []byte("   \n  \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := agent.BuildSystemPrompt(dir, []string{"EMPTY.md"})
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if got != "" {
		t.Fatalf("expected empty prompt, got %q", got)
	}
}

func TestBuildSystemPrompt_PermissionErrorPropagates(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("running as root: chmod 000 doesn't restrict")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "FORBIDDEN.md")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if _, err := agent.BuildSystemPrompt(dir, []string{"FORBIDDEN.md"}); err == nil {
		t.Fatalf("expected error reading non-readable file, got nil")
	}
}
