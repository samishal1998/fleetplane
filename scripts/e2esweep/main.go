// Command e2esweep deletes leftover E2E servers (ADR-015). It refuses to
// touch ANYTHING that does not carry fleetplane.io/test=1 — conservative
// destruction applies to test tooling too.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func main() {
	token := os.Getenv("HETZNER_TOKEN")
	if token == "" {
		fmt.Println("e2esweep: HETZNER_TOKEN unset; nothing to do")
		return
	}
	maxAge := 2 * time.Hour
	if os.Getenv("E2E_SWEEP_ALL") == "1" {
		maxAge = 0
	}
	client := hcloud.NewClient(hcloud.WithToken(token))
	ctx := context.Background()

	servers, err := client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: "fleetplane.io/test=1", PerPage: 50},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2esweep:", err)
		os.Exit(1)
	}
	swept, failed := 0, 0
	for _, srv := range servers {
		if srv.Labels["fleetplane.io/test"] != "1" {
			continue // double-check the label even after server-side filtering
		}
		if time.Since(srv.Created) < maxAge {
			fmt.Printf("e2esweep: keeping %s (%d): younger than %s\n", srv.Name, srv.ID, maxAge)
			continue
		}
		if _, _, err := client.Server.DeleteWithResult(ctx, srv); err != nil {
			fmt.Fprintf(os.Stderr, "e2esweep: delete %d: %v\n", srv.ID, err)
			failed++
			continue
		}
		fmt.Printf("e2esweep: deleted %s (%d)\n", srv.Name, srv.ID)
		swept++
	}
	fmt.Printf("e2esweep: %d deleted, %d failed, %d total test servers\n", swept, failed, len(servers))
	if failed > 0 {
		os.Exit(1)
	}
}
