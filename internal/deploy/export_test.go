package deploy

import (
	"context"

	"github.com/mgh3326/shiftgate/internal/config"
)

// DrainAndStop exposes drainAndStop for tests.
func (g *Gate) DrainAndStop(ctx context.Context, svc *config.Service, old config.Color) []string {
	return g.drainAndStop(ctx, svc, old)
}
