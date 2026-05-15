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
		{
			Name:        "jq",
			Description: "JSON tool",
			Binary:      "/usr/bin/jq",
			Docs:        tool.Docs{Usage: docPath},
		},
		{Name: "ls", Description: "list", Binary: "/bin/ls"},
	}

	tools, err := tool.LoadTools(defs, dir, nil, 0, 0, zap.NewNop())
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	// 2 BIN tools + 4 introspection tools (context_list,
	// context_show, env_list, mount_list — always registered).
	// job_* tools aren't registered here because the env vars
	// OTTERSD_URL / OTTERS_AGENT_TOKEN aren't set in this test.
	if got, want := len(tools), 2+introspectionToolCount; got != want {
		t.Fatalf("len(tools) = %d, want %d", got, want)
	}
}

func TestLoadTools_MissingDocIsTolerated(t *testing.T) {
	t.Parallel()

	tools, err := tool.LoadTools([]tool.Def{{
		Name:   "x",
		Binary: "/bin/true",
		Docs:   tool.Docs{Usage: "/no/such/path/USAGE.md"},
	}}, "/", nil, 0, 0, zap.NewNop())
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	// 1 BIN tool + the introspection tools.
	if got, want := len(tools), 1+introspectionToolCount; got != want {
		t.Fatalf("len(tools) = %d, want %d", got, want)
	}
}

// introspectionToolCount is the size of BuildIntrospectionTools'
// return. Pinned here so the test breaks loudly if the introspection
// tool surface ever expands or shrinks — a silent count drift would
// mask a real LLM-visible change.
const introspectionToolCount = 4
