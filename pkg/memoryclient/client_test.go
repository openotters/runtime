package memoryclient_test

import (
	"strings"
	"testing"

	"github.com/openotters/runtime/pkg/memoryclient"
)

func TestNew_NotConfiguredOnFirstUse(t *testing.T) {
	t.Parallel()
	c := memoryclient.New(memoryclient.Config{})
	if _, err := c.GetMessages(t.Context(), "s"); err == nil {
		t.Fatal("expected ErrNotConfigured when URL+token empty")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close on never-dialed client = %v, want nil", err)
	}
}

func TestFromEnv_BothMissing(t *testing.T) {
	t.Setenv(memoryclient.EnvDaemonURL, "")
	t.Setenv(memoryclient.EnvAgentToken, "")

	if _, ok := memoryclient.FromEnv(); ok {
		t.Fatal("FromEnv reported ok with both vars empty")
	}
}

func TestFromEnv_PartialMissing(t *testing.T) {
	t.Setenv(memoryclient.EnvDaemonURL, "unix:///tmp/x")
	t.Setenv(memoryclient.EnvAgentToken, "")

	if _, ok := memoryclient.FromEnv(); ok {
		t.Fatal("FromEnv reported ok with empty token")
	}
}

func TestFromEnv_BothSet(t *testing.T) {
	t.Setenv(memoryclient.EnvDaemonURL, "http://localhost:5050")
	t.Setenv(memoryclient.EnvAgentToken, "tok-abc")

	cfg, ok := memoryclient.FromEnv()
	if !ok {
		t.Fatal("FromEnv reported not-ok with both vars set")
	}
	if cfg.URL != "http://localhost:5050" || cfg.Token != "tok-abc" {
		t.Errorf("FromEnv = %+v, want URL+Token from env", cfg)
	}
}

func TestDial_BadURLSchemeSurfaces(t *testing.T) {
	t.Parallel()
	c := memoryclient.New(memoryclient.Config{URL: "ftp://nope", Token: "tok"})
	_, err := c.ListMessages(t.Context(), "s")
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("err = %v, want scheme error from dialTarget", err)
	}
}

func TestDial_UnixWithoutPath(t *testing.T) {
	t.Parallel()
	c := memoryclient.New(memoryclient.Config{URL: "unix://", Token: "tok"})
	_, err := c.ListMessages(t.Context(), "s")
	if err == nil {
		t.Fatal("expected error for empty unix path")
	}
}
