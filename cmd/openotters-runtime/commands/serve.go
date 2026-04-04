package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/merlindorin/go-shared/pkg/cmd"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	runtimev1 "github.com/openotters/runtime/api/v1"
	"github.com/openotters/runtime/internal/server"
)

type Serve struct {
	AgentConfig `embed:""`
	Addr        string `help:"gRPC listen address" default:":8080"`
}

func (s *Serve) Run(
	ctx context.Context,
	common *cmd.Commons,
	sqlite *cmd.SQLite,
) error {
	logger := common.MustLogger().Named("openotters-runtime")

	logger.Info("starting",
		zap.String("version", common.Version.Version()),
		zap.String("model", s.Model),
		zap.String("name", s.Name),
		zap.String("root", s.Root),
		zap.String("addr", s.Addr),
	)

	setup, err := s.AgentConfig.setup(ctx, sqlite, logger)
	if err != nil {
		return err
	}

	logger.Info("agent configured",
		zap.Int("tools", setup.toolCount),
		zap.Int("neighbors", len(s.Neighbors)),
	)

	srv := grpc.NewServer()
	runtimev1.RegisterAgentRuntimeServer(srv, server.NewGRPCServer(setup.svc, s.Name, s.Model))
	reflection.Register(srv)

	lis, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.Addr, err)
	}

	logger.Info("gRPC server listening", zap.String("addr", s.Addr))

	go func() {
		term := make(chan os.Signal, 1)
		signal.Notify(term, os.Interrupt, syscall.SIGTERM)

		select {
		case <-ctx.Done():
		case <-term:
		}

		logger.Info("shutting down")
		srv.GracefulStop()
	}()

	if err = srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}

	return nil
}
