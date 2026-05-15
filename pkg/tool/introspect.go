package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"gopkg.in/yaml.v3"
)

// BuildIntrospectionTools returns the four runtime-registered tools
// that let the model inspect its own declared context / envs / mounts
// out of agent.yaml: `context_list`, `context_show`, `env_list`,
// `mount_list`. Each reads /etc/agent.yaml (or wherever agentRoot
// points) at invocation time — agent.yaml is materialised once per
// run, so the data doesn't change between startup and tool call.
//
// agentRoot is the agent's filesystem root (`/` for the docker
// executor, `<agent-uuid-dir>` for system). The tools join against
// it to find etc/agent.yaml and etc/context/*.md.
//
// Env values are never returned — `env_list` surfaces keys +
// descriptions only. Mount host paths are never returned —
// `mount_list` surfaces targets + descriptions + the read-only flag.
// Both rules are by design: the agent.yaml on disk doesn't carry
// those fields in the first place, so the tools couldn't leak them
// even if asked.
func BuildIntrospectionTools(agentRoot string) []fantasy.AgentTool {
	return []fantasy.AgentTool{
		contextListTool(agentRoot),
		contextShowTool(agentRoot),
		envListTool(agentRoot),
		mountListTool(agentRoot),
	}
}

// introspectYAML is the minimal slice of agent.yaml the introspection
// tools care about. Keep it tight — adding fields here doesn't expose
// them to the model unless a tool actually renders them.
type introspectYAML struct {
	Context []struct {
		Name        string `yaml:"name"`
		File        string `yaml:"file"`
		Description string `yaml:"description,omitempty"`
	} `yaml:"context,omitempty"`
	Envs []struct {
		Key         string `yaml:"key"`
		Description string `yaml:"description,omitempty"`
	} `yaml:"envs,omitempty"`
	Mounts []struct {
		Target      string `yaml:"target"`
		Description string `yaml:"description,omitempty"`
		ReadOnly    bool   `yaml:"read_only,omitempty"`
	} `yaml:"mounts,omitempty"`
}

// loadIntrospectYAML reads etc/agent.yaml from the agent root.
// Returns a typed introspection view; unrelated fields are ignored.
// Errors propagate as ToolResponse.IsError so the model sees a clear
// "agent.yaml not readable" answer instead of a silent empty.
func loadIntrospectYAML(agentRoot string) (*introspectYAML, error) {
	path := filepath.Join(agentRoot, "etc", "agent.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var y introspectYAML
	if uErr := yaml.Unmarshal(data, &y); uErr != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, uErr)
	}
	return &y, nil
}

// emptyInput is the input shape for tools that take no parameters.
// The fantasy schema generator still needs a struct to produce an
// empty `properties: {}` JSON Schema, so this exists to satisfy that.
type emptyInput struct{}

// errToolResp surfaces a Go error as a ToolResponse the fantasy tool
// loop renders to the model — IsError true, the error string in
// Content. The outer return error is always nil because the tool
// loop treats a non-nil error as a runtime fault that aborts the
// turn; we want the model to see the message and adapt.
func errToolResp(err error) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{IsError: true, Content: err.Error()}, nil
}

// contextNameInput is the input shape for context_show. The model
// passes a Name (e.g. "SOUL"); the tool looks it up in the
// context: list and returns the file's content.
type contextNameInput struct {
	Name string `json:"name" jsonschema:"description=The context file's short name as declared in agent.yaml (e.g. SOUL)"`
}

func contextListTool(agentRoot string) fantasy.AgentTool {
	desc := "List the context files declared in agent.yaml — name, " +
		"file path, and a short description. These are the markdown " +
		"files loaded into your system prompt at start-up. Use " +
		"`context_show NAME` to read one in full."
	return fantasy.NewAgentTool(
		"context_list",
		desc,
		func(_ context.Context, _ emptyInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			y, err := loadIntrospectYAML(agentRoot)
			if err != nil {
				return errToolResp(err)
			}
			if len(y.Context) == 0 {
				return fantasy.ToolResponse{Content: "No context files declared.\n"}, nil
			}
			var b strings.Builder
			b.WriteString("| Name | File | Description |\n")
			b.WriteString("|------|------|-------------|\n")
			for _, c := range y.Context {
				cellDesc := c.Description
				if cellDesc == "" {
					cellDesc = "-"
				}
				fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", c.Name, c.File, cellDesc)
			}
			return fantasy.ToolResponse{Content: b.String()}, nil
		},
	)
}

func contextShowTool(agentRoot string) fantasy.AgentTool {
	desc := "Show the full content of one context file. The name must " +
		"match a declared entry in agent.yaml's context: list (use " +
		"context_list to see them)."
	return fantasy.NewAgentTool(
		"context_show",
		desc,
		func(_ context.Context, in contextNameInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if in.Name == "" {
				return fantasy.ToolResponse{
					IsError: true,
					Content: "name is required (e.g. context_show SOUL)",
				}, nil
			}
			y, err := loadIntrospectYAML(agentRoot)
			if err != nil {
				return errToolResp(err)
			}

			var file string
			for _, c := range y.Context {
				if c.Name == in.Name {
					file = c.File
					break
				}
			}
			if file == "" {
				names := make([]string, 0, len(y.Context))
				for _, c := range y.Context {
					names = append(names, c.Name)
				}
				return fantasy.ToolResponse{
					IsError: true,
					Content: fmt.Sprintf("no context named %q; declared: %s", in.Name, strings.Join(names, ", ")),
				}, nil
			}

			// File paths in agent.yaml are absolute from the agent
			// root (e.g. /etc/context/SOUL.md). Join handles the
			// leading slash — `filepath.Join("/r", "/etc/...")` →
			// `/r/etc/...`. On docker (root = "/") the join is a
			// no-op; on system it produces the chroot-relative
			// real path.
			data, err := os.ReadFile(filepath.Join(agentRoot, file))
			if err != nil {
				return errToolResp(err)
			}
			return fantasy.ToolResponse{Content: string(data)}, nil
		},
	)
}

func envListTool(agentRoot string) fantasy.AgentTool {
	desc := "List the environment variable keys this agent declares, " +
		"with a short description for each. Values are never returned " +
		"— they're available to your tool subprocesses via the spawn " +
		"env (e.g. `echo $KEY` inside `sh -c` works); this tool is the " +
		"structured catalogue of what's expected."
	return fantasy.NewAgentTool(
		"env_list",
		desc,
		func(_ context.Context, _ emptyInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			y, err := loadIntrospectYAML(agentRoot)
			if err != nil {
				return errToolResp(err)
			}
			if len(y.Envs) == 0 {
				return fantasy.ToolResponse{Content: "No environment variables declared.\n"}, nil
			}
			var b strings.Builder
			b.WriteString("| Key | Description |\n")
			b.WriteString("|-----|-------------|\n")
			for _, e := range y.Envs {
				cellDesc := e.Description
				if cellDesc == "" {
					cellDesc = "-"
				}
				fmt.Fprintf(&b, "| `%s` | %s |\n", e.Key, cellDesc)
			}
			return fantasy.ToolResponse{Content: b.String()}, nil
		},
	)
}

func mountListTool(agentRoot string) fantasy.AgentTool {
	desc := "List the bind mounts this agent has — target path inside " +
		"the agent, description, and whether the mount is read-only. " +
		"Host source paths are never returned (the operator-side " +
		"detail isn't on disk in agent.yaml)."
	return fantasy.NewAgentTool(
		"mount_list",
		desc,
		func(_ context.Context, _ emptyInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			y, err := loadIntrospectYAML(agentRoot)
			if err != nil {
				return errToolResp(err)
			}
			if len(y.Mounts) == 0 {
				return fantasy.ToolResponse{Content: "No bind mounts.\n"}, nil
			}
			var b strings.Builder
			b.WriteString("| Target | Description | Read-only |\n")
			b.WriteString("|--------|-------------|-----------|\n")
			for _, m := range y.Mounts {
				cellDesc := m.Description
				if cellDesc == "" {
					cellDesc = "-"
				}
				ro := "no"
				if m.ReadOnly {
					ro = "yes"
				}
				fmt.Fprintf(&b, "| `%s` | %s | %s |\n", m.Target, cellDesc, ro)
			}
			return fantasy.ToolResponse{Content: b.String()}, nil
		},
	)
}
