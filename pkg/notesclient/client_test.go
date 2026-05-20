package notesclient_test

import (
	"strings"
	"testing"

	"github.com/openotters/runtime/pkg/notesclient"
)

func TestNew_NotConfiguredOnFirstUse(t *testing.T) {
	t.Parallel()
	c := notesclient.New(notesclient.Config{})
	if _, err := c.List(t.Context()); err == nil {
		t.Fatal("expected ErrNotConfigured when URL+token empty")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close on never-dialed client = %v, want nil", err)
	}
}

func TestFromEnv_BothMissing(t *testing.T) {
	t.Setenv(notesclient.EnvDaemonURL, "")
	t.Setenv(notesclient.EnvAgentToken, "")

	if _, ok := notesclient.FromEnv(); ok {
		t.Fatal("FromEnv reported ok with both vars empty")
	}
}

func TestFromEnv_BothSet(t *testing.T) {
	t.Setenv(notesclient.EnvDaemonURL, "http://localhost:5050")
	t.Setenv(notesclient.EnvAgentToken, "tok-abc")

	cfg, ok := notesclient.FromEnv()
	if !ok || cfg.URL != "http://localhost:5050" || cfg.Token != "tok-abc" {
		t.Fatalf("FromEnv = (%+v, %v)", cfg, ok)
	}
}

func TestDial_BadURLSchemeSurfaces(t *testing.T) {
	t.Parallel()
	c := notesclient.New(notesclient.Config{URL: "ftp://nope", Token: "tok"})
	_, err := c.List(t.Context())
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("err = %v, want scheme error from dialTarget", err)
	}
}
