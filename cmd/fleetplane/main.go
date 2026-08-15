// Command fleetplane is the Fleetplane control plane: a single binary that
// runs the server (`fleetplane serve`) and doubles as the CLI client for a
// running control plane (ADR-006).
package main

import (
	"os"

	"github.com/samishal1998/fleetplane/internal/cli"
)

// version is set via -ldflags "-X main.version=..." on release builds.
var version = "dev"

func main() { os.Exit(cli.Execute(version)) }
