package main

import (
	"context"
	"log/slog"

	"github.com/samimishal/fleetplane/internal/boot"
)

// bootRun is indirected for testability of run() without a real server.
var bootRun = func(ctx context.Context, cfgPath string, log *slog.Logger) error {
	return boot.Run(ctx, cfgPath, log)
}
