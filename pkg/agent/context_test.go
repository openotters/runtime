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

	got, err := agent.BuildSystemPrompt(dir, []agent.ContextFile{
		{Name: "AGENTS", Path: "AGENTS.md"},
		{Name: "MISSING", Path: "MISSING.md"},
		{Name: "TOOLS", Path: "TOOLS.md"},
	})
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	// Headers must use the supplied Name, not the file path — otherwise
	// the model reads "## /etc/context/AGENT.md" and treats the section
	// as a path reference instead of a document title.
	if !strings.Contains(got, "## AGENTS\n") || !strings.Contains(got, "## TOOLS\n") {
		t.Fatalf("missing Name-based headers in prompt:\n%s", got)
	}

	if strings.Contains(got, "## AGENTS.md") || strings.Contains(got, "## TOOLS.md") {
		t.Fatalf("file basename leaked into header (Name should take precedence):\n%s", got)
	}

	if !strings.Contains(got, "\n\n---\n\n") {
		t.Fatalf("missing separator between sections:\n%s", got)
	}

	if strings.Contains(got, "MISSING") {
		t.Fatalf("non-existent file leaked into prompt:\n%s", got)
	}
}

func TestBuildSystemPrompt_EmptyNameFallsBackToBasename(t *testing.T) {
	t.Parallel()

	// When the daemon supplies a Path but no Name, the section still
	// gets a sensible header — the basename of the file. Failing this
	// would emit "## " with nothing after it and leave the model
	// guessing what the block is.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "RAW.md"), []byte("body\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := agent.BuildSystemPrompt(dir, []agent.ContextFile{{Path: "RAW.md"}})
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if !strings.Contains(got, "## RAW.md") {
		t.Fatalf("basename fallback header missing:\n%s", got)
	}
}

func TestBuildSystemPrompt_EmptyFilesAreSkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "EMPTY.md"), []byte("   \n  \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := agent.BuildSystemPrompt(dir, []agent.ContextFile{{Name: "EMPTY", Path: "EMPTY.md"}})
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

	_, err := agent.BuildSystemPrompt(dir, []agent.ContextFile{{Name: "FORBIDDEN", Path: "FORBIDDEN.md"}})
	if err == nil {
		t.Fatalf("expected error reading non-readable file, got nil")
	}
}
