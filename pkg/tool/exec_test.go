//nolint:testpackage // tests the unexported executor and env builder
package tool

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"charm.land/fantasy"
	"go.uber.org/zap"
)

func TestExecutorEnv_PrependsSandboxBin(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("FOO", "bar")

	e := newExecutor("ls", nil, "/sandbox", zap.NewNop())
	env := e.env()

	var pathLine, fooLine string
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "PATH="):
			pathLine = kv
		case strings.HasPrefix(kv, "FOO="):
			fooLine = kv
		}
	}

	want := "PATH=/sandbox/usr/bin:/usr/bin:/bin"
	if pathLine != want {
		t.Errorf("PATH = %q, want %q", pathLine, want)
	}

	if fooLine != "FOO=bar" {
		t.Errorf("env did not pass through FOO; got %q", fooLine)
	}
}

func TestExecutorEnv_PrependsWhenNoPATHInEnv(t *testing.T) {
	t.Setenv("PATH", "")
	os.Unsetenv("PATH")

	e := newExecutor("ls", nil, "/sandbox", zap.NewNop())
	env := e.env()

	found := false
	for _, kv := range env {
		if kv == "PATH=/sandbox/usr/bin" {
			found = true
		}
	}

	if !found {
		t.Errorf("expected synthesized PATH=/sandbox/usr/bin in env, got %v", env)
	}
}

func TestExecutorRun_ArgsBecomeArgv(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("/bin/echo"); err != nil {
		t.Skipf("/bin/echo unavailable: %v", err)
	}

	// Static config args ("hello") are prepended to the call args
	// ("world", "and friends"), giving argv = [hello world and friends].
	e := newExecutor("/bin/echo", []string{"hello"}, "", zap.NewNop())

	resp, err := e.Run(context.Background(),
		Input{Args: []string{"world", "and", "friends"}}, fantasy.ToolCall{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if resp.IsError {
		t.Fatalf("expected success, got IsError=true content=%q", resp.Content)
	}

	if got := strings.TrimSpace(resp.Content); got != "hello world and friends" {
		t.Errorf("Content = %q, want %q", got, "hello world and friends")
	}
}

func TestExecutorRun_StdinIsPiped(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("/bin/cat"); err != nil {
		t.Skipf("/bin/cat unavailable: %v", err)
	}

	// /bin/cat with no args reads its input from stdin.
	e := newExecutor("/bin/cat", nil, "", zap.NewNop())

	resp, err := e.Run(context.Background(),
		Input{Stdin: "stdin payload\n"}, fantasy.ToolCall{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if resp.IsError {
		t.Fatalf("expected success, got IsError=true content=%q", resp.Content)
	}

	if got := strings.TrimSpace(resp.Content); got != "stdin payload" {
		t.Errorf("Content = %q, want %q", got, "stdin payload")
	}
}

func TestExecutorRun_ArgsAndStdinTogether(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}

	// `sh -c "cat -; echo done"` echoes stdin then prints "done" — exercises
	// args (passed through Input.Args) and stdin together.
	e := newExecutor("/bin/sh", nil, "", zap.NewNop())

	resp, err := e.Run(context.Background(),
		Input{
			Args:  []string{"-c", "cat -; echo done"},
			Stdin: "from stdin\n",
		}, fantasy.ToolCall{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if resp.IsError {
		t.Fatalf("expected success, got IsError=true content=%q", resp.Content)
	}

	want := "from stdin\ndone"
	if got := strings.TrimSpace(resp.Content); got != want {
		t.Errorf("Content = %q, want %q", got, want)
	}
}

func TestExecutorRun_EmptyCallIsRejected(t *testing.T) {
	t.Parallel()

	e := newExecutor("/bin/echo", nil, "", zap.NewNop())

	resp, err := e.Run(context.Background(), Input{}, fantasy.ToolCall{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !resp.IsError {
		t.Fatalf("expected IsError=true for empty call; content=%q", resp.Content)
	}

	if !strings.Contains(resp.Content, "args, stdin, or both") {
		t.Errorf("expected guidance in error content; got %q", resp.Content)
	}
}

func TestExecutorRun_NonZeroExitMarksError(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}

	e := newExecutor("/bin/sh", []string{"-c", "echo oops >&2; exit 1"}, "", zap.NewNop())

	resp, err := e.Run(context.Background(),
		Input{Args: []string{"unused"}}, fantasy.ToolCall{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !resp.IsError {
		t.Fatalf("expected IsError=true; content=%q", resp.Content)
	}

	if !strings.Contains(resp.Content, "oops") {
		t.Errorf("expected stderr in content; got %q", resp.Content)
	}
}
