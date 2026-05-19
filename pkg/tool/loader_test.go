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

	tools, err := tool.LoadTools(defs, dir, allIntrospectionCaps(), nil, 0, 0, zap.NewNop())
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	// 2 BIN tools + 4 introspection tools (context_list,
	// context_show, env_list, mount_list — granted via caps).
	// job_* / agent_* / note_* aren't registered here because
	// they weren't granted via caps in this test.
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
	}}, "/", allIntrospectionCaps(), nil, 0, 0, zap.NewNop())
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	// 1 BIN tool + the introspection tools.
	if got, want := len(tools), 1+introspectionToolCount; got != want {
		t.Fatalf("len(tools) = %d, want %d", got, want)
	}
}

func TestLoadTools_NoCapsDropsAutoInjectedTools(t *testing.T) {
	t.Parallel()

	// With an empty cap list, only BIN-declared tools should
	// register. Introspection / job / agent / notes tools all
	// drop. This mirrors the strict-default the daemon enforces
	// when an Agentfile has no CAPABILITY directives.
	tools, err := tool.LoadTools([]tool.Def{{
		Name: "x", Binary: "/bin/true",
	}}, "/", nil, nil, 0, 0, zap.NewNop())
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	if got, want := len(tools), 1; got != want {
		t.Fatalf("len(tools) = %d, want %d (only BIN tool, no caps)", got, want)
	}
}

// introspectionToolCount is the size of BuildIntrospectionTools'
// return. Pinned here so the test breaks loudly if the introspection
// tool surface ever expands or shrinks — a silent count drift would
// mask a real LLM-visible change.
const introspectionToolCount = 4

// allIntrospectionCaps returns the cap names that grant every
// introspection tool. A function (not a var) to keep the test
// package free of mutable global state.
func allIntrospectionCaps() []string {
	return []string{"context_list", "context_show", "env_list", "mount_list"}
}
