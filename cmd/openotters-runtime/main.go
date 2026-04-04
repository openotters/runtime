package main

import (
	"context"

	"github.com/alecthomas/kong"
	kongyaml "github.com/alecthomas/kong-yaml"
	c "github.com/merlindorin/go-shared/pkg/cmd"

	"github.com/openotters/runtime/cmd/openotters-runtime/commands"
)

const (
	name        = "openotters-runtime"
	description = "openotters autonomous AI agent runtime"
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
		kong.DefaultEnvars("OPENOTTERS"),
		kong.Configuration(kongyaml.Loader),
	)

	ctx.BindTo(context.Background(), (*context.Context)(nil))
	ctx.FatalIfErrorf(ctx.Run(cli.Commons, cli.SQLite))
}

type CMD struct {
	*c.Commons
	*c.SQLite `embed:""`
	*c.Config `embed:""`

	Serve  *commands.Serve  `cmd:"" help:"Start the agent runtime"`
	Prompt *commands.Prompt `cmd:"" help:"Send a single prompt and display streamed steps (debug)"`
}
