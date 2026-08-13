// Command fleetplane is the Fleetplane control plane: a single binary that
// serves the API, runs the reconciler and operation engine, and doubles as
// the CLI client for a running control plane.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

// version is set via -ldflags "-X main.version=..." on release builds.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("fleetplane %s (%s)\n", version, goVersion())
		return
	}
	fmt.Fprintln(os.Stderr, "fleetplane: no subcommands implemented yet (increment I0)")
	os.Exit(2)
}

func goVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.GoVersion
	}
	return "unknown"
}
