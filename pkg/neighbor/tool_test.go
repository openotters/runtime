package neighbor_test

import (
	"testing"

	"go.uber.org/zap"

	"github.com/openotters/runtime/pkg/neighbor"
)

func TestBuildNeighborTools_OneToolPerNeighbor(t *testing.T) {
	t.Parallel()

	tools := neighbor.BuildNeighborTools([]neighbor.Config{
		{Name: "alpha", URL: "http://example.com"},
		{Name: "beta", URL: "http://example.com"},
	}, zap.NewNop())

	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(tools))
	}
}

func TestBuildNeighborTools_EmptyConfigEmptyTools(t *testing.T) {
	t.Parallel()

	tools := neighbor.BuildNeighborTools(nil, zap.NewNop())

	if len(tools) != 0 {
		t.Fatalf("len(tools) = %d, want 0", len(tools))
	}
}
