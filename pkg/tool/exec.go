package tool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"charm.land/fantasy"
	"go.uber.org/zap"
)

// Input is what the LLM writes when it calls a BIN tool: a single
// free-form command string. The runtime shell-splits this into argv
// and execs the tool binary directly — no JSON envelope, no stdin
// stuffing, no wrapper per tool. The field stays named "input" so
// existing LLM tool-call schemas advertised via jsonschema reflection
// keep working without a tool-call migration.
type Input struct {
	Input string `json:"input" jsonschema:"description=Command string passed to the tool; shell-split into argv"`
}

type executor struct {
	binary string
	args   []string
	dir    string
	logger *zap.Logger
}

func newExecutor(binary string, args []string, dir string, logger *zap.Logger) *executor {
	return &executor{binary: binary, args: args, dir: dir, logger: logger}
}

// Run execs the tool binary with argv = static config args + the
// shell-split user input. Stdout becomes the tool response content;
// stderr + the exit error surface to the LLM as IsError=true.
func (e *executor) Run(ctx context.Context, input Input, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	argv := append(append([]string{}, e.args...), splitArgs(input.Input)...)

	cmd := exec.CommandContext(ctx, e.binary, argv...) //nolint:gosec // binary is from trusted config
	cmd.Dir = e.dir
	cmd.Env = e.env()

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		e.logger.Warn("tool execution failed",
			zap.String("binary", e.binary),
			zap.Strings("argv", argv),
			zap.Error(err),
			zap.String("stderr", stderr.String()),
		)

		return fantasy.ToolResponse{
			IsError: true,
			Content: fmt.Sprintf("execution error: %s\n%s", err, stderr.String()),
		}, nil
	}

	return fantasy.ToolResponse{Content: stdout.String()}, nil
}

// env builds the environment the tool subprocess runs with. We
// inherit the runtime's env (so API keys, locale, TMPDIR and so on
// pass through) and then prepend the sandbox's usr/bin directory to
// PATH. That lets a BIN tool spawn another BIN by name — e.g.
// `sh -c "otters ps | tee /workspace/out.txt"` resolves `otters`
// and `tee` without the agent having to know the chroot's
// absolute path.
func (e *executor) env() []string {
	base := os.Environ()
	sandboxBin := filepath.Join(e.dir, "usr", "bin")

	out := make([]string, 0, len(base)+1)
	replaced := false

	for _, kv := range base {
		if len(kv) > 5 && kv[:5] == "PATH=" {
			out = append(out, "PATH="+sandboxBin+string(os.PathListSeparator)+kv[5:])
			replaced = true

			continue
		}

		out = append(out, kv)
	}

	if !replaced {
		out = append(out, "PATH="+sandboxBin)
	}

	return out
}
