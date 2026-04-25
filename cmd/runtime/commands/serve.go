package commands

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/merlindorin/go-shared/pkg/cmd"
	"github.com/openotters/runtime/internal"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	runtimev1 "github.com/openotters/agentfile/agent/api/v1"
)

type Serve struct {
	AgentConfig `embed:""`
}

func (s *Serve) Run(
	ctx context.Context,
	common *cmd.Commons,
	sqlite *cmd.SQLite,
) error {
	logger := common.MustLogger().Named("runtime")

	logger.Info("starting",
		zap.String("version", common.Version.Version()),
		zap.String("model", s.Model),
		zap.String("name", s.Name),
		zap.String("root", s.Root),
		zap.String("addr", s.Addr),
	)

	setup, err := s.setup(ctx, sqlite, logger)
	if err != nil {
		return err
	}

	logger.Info("agent configured",
		zap.Int("tools", setup.toolCount),
		zap.Int("neighbors", len(s.Neighbors)),
	)

	srv := grpc.NewServer()
	runtimev1.RegisterAgentRuntimeServer(srv, internal.NewGRPCServer(setup.svc, s.Name, s.Model))
	reflection.Register(srv)

	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.Addr, err)
	}

	logger.Info("gRPC server listening", zap.String("addr", s.Addr))

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		srv.GracefulStop()
	}()

	if err = srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}

	return nil
}
