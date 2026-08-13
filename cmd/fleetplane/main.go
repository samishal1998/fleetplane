// Command fleetplane is the Fleetplane control plane: a single binary that
// serves the API and runs the reconciler/operation engine (`fleetplane
// serve`), and doubles as the CLI client for a running control plane.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

// version is set via -ldflags "-X main.version=..." on release builds.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Printf("fleetplane %s (%s)\n", version, goVersion())
		return 0
	case "serve":
		return serve(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "fleetplane: unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to the fleetplane config file (required)")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "fleetplane serve: --config is required")
		return 2
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "fleetplane serve: invalid --log-level %q\n", *logLevel)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// First signal: graceful shutdown. Second signal: immediate exit 130.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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

	if err := bootRun(ctx, *cfgPath, log); err != nil {
		log.Error("fleetplane exited with error", "error", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: fleetplane <command>

commands:
  serve --config PATH   run the control plane
  version               print version
`)
}

func goVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.GoVersion
	}
	return "unknown"
}
