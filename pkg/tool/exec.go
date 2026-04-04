package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"charm.land/fantasy"
	"go.uber.org/zap"
)

type Input struct {
	Input string `json:"input" jsonschema:"description=The input to pass to the tool"`
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

func (e *executor) Run(ctx context.Context, input Input, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return fantasy.ToolResponse{IsError: true, Content: fmt.Sprintf("marshal input: %s", err)}, nil
	}

	cmd := exec.CommandContext(ctx, e.binary, e.args...) //nolint:gosec // binary is from trusted config
	cmd.Dir = e.dir
	cmd.Stdin = bytes.NewReader(inputJSON)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err = cmd.Run(); err != nil {
		e.logger.Warn("tool execution failed",
			zap.String("binary", e.binary),
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
