package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
	kongyaml "github.com/alecthomas/kong-yaml"
	c "github.com/merlindorin/go-shared/pkg/cmd"

	"github.com/openotters/runtime/cmd/runtime/commands"
)

const (
	name        = "runtime"
	description = "otters autonomous AI agent runtime"
)

//nolint:gochecknoglobals // set by ldflags at build time
var (
	version     = "dev"
	commit      = "dirty"
	date        = "latest"
	buildSource = "source"
)

func main() {
	cli := CMD{
		Commons: &c.Commons{
			Version: c.NewVersion(name, version, commit, buildSource, date),
		},
		SQLite: c.NewSQLite(),
		Config: c.NewConfig(),

		Serve:  &commands.Serve{},
		Prompt: &commands.Prompt{},
	}

	ctx := kong.Parse(
		&cli,
		kong.Name(name),
		kong.Description(description),
		kong.UsageOnError(),
		kong.DefaultEnvars("OTTERS"),
		kong.Configuration(kongyaml.Loader),
	)

	// Signal-wired root context: ottersd sends SIGTERM to this
	// subprocess on agent Stop/Remove; SIGINT catches Ctrl-C when
	// `runtime serve` is launched directly for debugging.
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx.BindTo(runCtx, (*context.Context)(nil))
	ctx.FatalIfErrorf(ctx.Run(cli.Commons, cli.SQLite))
}

type CMD struct {
	*c.Commons
	*c.SQLite `embed:""`
	*c.Config `embed:""`

	Serve  *commands.Serve  `cmd:"" help:"Start the agent runtime"`
	Prompt *commands.Prompt `cmd:"" help:"Send a single prompt and display streamed steps (debug)"`
}
