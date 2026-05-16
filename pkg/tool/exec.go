package tool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"go.uber.org/zap"
)

// Input is the BIN tool call shape exposed to the LLM. Fantasy reflects
// this struct into a JSON schema, so the model sees `args` and `stdin`
// as named fields and picks whichever the tool needs.
//
// Both fields are optional. Tools that take only argv use `args` and
// ignore `stdin`; tools whose meaningful input flows over stdin
// (`yaegi run -`, `pandoc`, `markitdown`, …) use `stdin` (often
// alongside a small `args` like `["run", "-"]`). At least one of them
// is required — empty calls are rejected so the model gets immediate
// feedback rather than the binary's own help text.
type Input struct {
	Args  []string `json:"args,omitempty"  jsonschema:"description=Positional arguments passed to the binary as argv. Use this for flags, paths, subcommands."`
	Stdin string   `json:"stdin,omitempty" jsonschema:"description=Content piped to the binary's stdin. Use for tools that read source/data from stdin (e.g. yaegi run -, jq with no input file)."`
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

// Run execs the tool binary with argv = static config args + Input.Args
// and pipes Input.Stdin to the process. Stdout becomes the tool
// response content; stderr + the exit error surface to the LLM as
// IsError=true.
func (e *executor) Run(ctx context.Context, in Input, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if len(in.Args) == 0 && in.Stdin == "" {
		return fantasy.ToolResponse{
			IsError: true,
			Content: "tool call is empty: provide args, stdin, or both",
		}, nil
	}

	argv := append(append([]string{}, e.args...), in.Args...)

	cmd := exec.CommandContext(ctx, e.binary, argv...) //nolint:gosec // binary is from trusted config
	cmd.Dir = e.dir
	cmd.Env = e.env()

	if in.Stdin != "" {
		cmd.Stdin = strings.NewReader(in.Stdin)
	}

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		e.logger.Warn("tool execution failed",
			zap.String("binary", e.binary),
			zap.Strings("argv", argv),
			zap.Bool("stdin", in.Stdin != ""),
			zap.Error(err),
			zap.String("stderr", stderr.String()),
		)

		return fantasy.ToolResponse{
			IsError: true,
			Content: fmt.Sprintf("execution error: %s\n%s", err, stderr.String()),
		}, nil
	}

	// Empty stdout on a successful exit is ambiguous to the model:
	// it reads "no content" as "did the call even happen?" and
	// retries the same tool call. Synthesise a success sentinel so
	// the model has a positive signal to anchor on. Stderr is
	// surfaced when present (tools that warn but still succeed).
	content := stdout.String()
	if content == "" {
		if errOut := strings.TrimSpace(stderr.String()); errOut != "" {
			content = fmt.Sprintf("(exit 0, no stdout; stderr: %s)", errOut)
		} else {
			content = "(exit 0, no output)"
		}
	}

	return fantasy.ToolResponse{Content: content}, nil
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
