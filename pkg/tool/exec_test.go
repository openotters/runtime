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

func TestExecutorRun_StdoutBecomesContent(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("/bin/echo"); err != nil {
		t.Skipf("/bin/echo unavailable: %v", err)
	}

	e := newExecutor("/bin/echo", []string{"hello"}, "", zap.NewNop())

	resp, err := e.Run(context.Background(), Input{Input: "world"}, fantasy.ToolCall{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if resp.IsError {
		t.Fatalf("expected success, got IsError=true content=%q", resp.Content)
	}

	if got := strings.TrimSpace(resp.Content); got != "hello world" {
		t.Errorf("Content = %q, want %q", got, "hello world")
	}
}

func TestExecutorRun_NonZeroExitMarksError(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}

	e := newExecutor("/bin/sh", []string{"-c", "echo oops >&2; exit 1"}, "", zap.NewNop())

	resp, err := e.Run(context.Background(), Input{Input: ""}, fantasy.ToolCall{})
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
