package notes_test

import (
	"strings"
	"testing"
	"time"

	"github.com/openotters/runtime/pkg/notes"
)

func TestRenderPromptSection_Empty(t *testing.T) {
	t.Parallel()

	if got := notes.RenderPromptSection(nil); got != "" {
		t.Errorf("empty list rendered non-empty: %q", got)
	}
	if got := notes.RenderPromptSection([]notes.Note{}); got != "" {
		t.Errorf("zero-len slice rendered non-empty: %q", got)
	}
}

func TestRenderPromptSection_Shape(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	rendered := notes.RenderPromptSection([]notes.Note{
		{Key: "k8s-cluster", Preview: "homelab, 3 nodes", UpdatedAt: now.Add(-5 * time.Minute)},
		{Key: "kubeconfig", Preview: "/kubeconfig.yaml", UpdatedAt: now.Add(-2 * 24 * time.Hour)},
	})

	for _, needle := range []string{
		"## Notes",
		"| Key | Updated | Preview |",
		"|-----|---------|---------|",
		"`k8s-cluster`",
		"homelab, 3 nodes",
		"`kubeconfig`",
		"/kubeconfig.yaml",
		"Notes persist across sessions",
		"`note_show <key>`",
	} {
		if !strings.Contains(rendered, needle) {
			t.Errorf("missing %q in output:\n%s", needle, rendered)
		}
	}
}

func TestRenderPromptSection_PipesEscaped(t *testing.T) {
	t.Parallel()

	// A preview that happens to contain a pipe character must not
	// break the markdown table layout. The renderer escapes it.
	out := notes.RenderPromptSection([]notes.Note{
		{Key: "weird", Preview: "a | b | c", UpdatedAt: time.Now()},
	})
	if !strings.Contains(out, `a \| b \| c`) {
		t.Errorf("pipes not escaped:\n%s", out)
	}
}

func TestRenderPromptSection_NewlinesFlattened(t *testing.T) {
	t.Parallel()

	// Defensive: derivePreview already collapses newlines on the
	// way in, but if a caller hand-builds a Note with a multi-line
	// preview, the renderer must still produce a single table row.
	out := notes.RenderPromptSection([]notes.Note{
		{Key: "multi", Preview: "line1\nline2", UpdatedAt: time.Now()},
	})
	if strings.Contains(out, "line1\nline2") {
		t.Errorf("newline leaked into table cell:\n%s", out)
	}
	if !strings.Contains(out, "line1 line2") {
		t.Errorf("newline-flattened cell missing:\n%s", out)
	}
}
