package main

// Compiled-in providers (ADR-013). This is the ONLY file touched to add or
// remove a provider from a distribution — never a central switch statement.
import (
	_ "github.com/samishal1998/fleetplane/pkg/kinds/volume"
	_ "github.com/samishal1998/fleetplane/providers/digitalocean"
	_ "github.com/samishal1998/fleetplane/providers/fake"
	_ "github.com/samishal1998/fleetplane/providers/hetzner"
)
