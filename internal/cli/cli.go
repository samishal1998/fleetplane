// Package cli implements the fleetplane command tree (ADR-006). Client
// commands speak ONLY to the HTTP API via pkg/apiclient; `serve` runs the
// control plane in-process.
package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/samimishal/fleetplane/internal/boot"
	"github.com/samimishal/fleetplane/pkg/apiclient"
)

// Exit codes (ADR-006).
const (
	ExitOK       = 0
	ExitRuntime  = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitAuth     = 4
	ExitConflict = 5
	ExitWatch    = 6
	ExitServer   = 7
)

type root struct {
	addr   string
	token  string
	output string
}

func (r *root) client() *apiclient.Client { return apiclient.New(r.addr, r.token) }

// Execute runs the CLI and returns the process exit code.
func Execute(version string) int {
	r := &root{}
	cmd := &cobra.Command{
		Use:           "fleetplane",
		Short:         "Fleetplane — agentless control plane for infrastructure fleets",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	defaultAddr := os.Getenv("FLEETPLANE_ADDR")
	if defaultAddr == "" {
		defaultAddr = "http://127.0.0.1:8080"
	}
	cmd.PersistentFlags().StringVar(&r.addr, "addr", defaultAddr, "Fleetplane API address (env FLEETPLANE_ADDR)")
	cmd.PersistentFlags().StringVar(&r.token, "token", os.Getenv("FLEETPLANE_TOKEN"), "API token (env FLEETPLANE_TOKEN)")
	cmd.PersistentFlags().StringVarP(&r.output, "output", "o", "table", "output format: table|json")

	cmd.AddCommand(versionCmd(version), serveCmd(), resourcesCmd(r))

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetplane:", err)
		return exitCode(err)
	}
	return ExitOK
}

func exitCode(err error) int {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == http.StatusNotFound:
			return ExitNotFound
		case apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden:
			return ExitAuth
		case apiErr.Status == http.StatusConflict:
			return ExitConflict
		case apiErr.Status >= 500:
			return ExitServer
		}
	}
	return ExitRuntime
}

func versionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, _ []string) {
			goVer := "unknown"
			if bi, ok := debug.ReadBuildInfo(); ok {
				goVer = bi.GoVersion
			}
			fmt.Fprintf(cmd.OutOrStdout(), "fleetplane %s (%s)\n", version, goVer)
		},
	}
}

func serveCmd() *cobra.Command {
	var cfgPath, logLevel string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the control plane",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var level slog.Level
			if err := level.UnmarshalText([]byte(logLevel)); err != nil {
				return fmt.Errorf("invalid --log-level %q", logLevel)
			}
			log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

			// First signal: graceful shutdown; second: immediate exit 130.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
				sig := make(chan os.Signal, 1)
				signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
				<-sig
				fmt.Fprintln(os.Stderr, "fleetplane: forced exit")
				os.Exit(130)
			}()
			return boot.Run(ctx, cfgPath, log)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "path to the config file (required)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug|info|warn|error")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
