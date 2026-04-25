package tool_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/openotters/runtime/pkg/tool"
)

func TestLoadTools_DescriptionAppendsDocBody(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	docPath := filepath.Join(dir, "USAGE.md")
	if err := os.WriteFile(docPath, []byte("\n  Pass --json to format.\n"), 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}

	defs := []tool.Def{
		{Name: "jq", Description: "JSON tool", Binary: "/usr/bin/jq", Doc: docPath},
		{Name: "ls", Description: "list", Binary: "/bin/ls"},
	}

	tools, err := tool.LoadTools(defs, dir, zap.NewNop())
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(tools))
	}
}

func TestLoadTools_MissingDocIsTolerated(t *testing.T) {
	t.Parallel()

	tools, err := tool.LoadTools([]tool.Def{{
		Name:   "x",
		Binary: "/bin/true",
		Doc:    "/no/such/path/USAGE.md",
	}}, "/", zap.NewNop())
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(tools))
	}
}
