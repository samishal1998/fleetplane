package tests

// Env-gated E2E against a REAL Google Cloud project (ADR-015): double gate
// (FLEETPLANE_E2E=1 ∧ GCP_E2E_PROJECT ∧ GCP_E2E_CREDENTIALS_FILE), dedicated
// throwaway project only, every instance labeled fleetplane.io/test (the
// driver encodes it as fp-test, providers/gcp/labels.go), cleanup guaranteed
// by t.Cleanup plus the CI sweeper.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/pkg/sdk/conformance"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
	"github.com/samishal1998/fleetplane/providers/gcp"
)

// us-central1-a: Tier-1 (cheapest) GCE pricing and one of the e2-micro
// free-tier regions, so a short-lived test machine costs ~nothing.
const gcpE2EDefaultZone = "us-central1-a"

func gcpE2EZone() string {
	if z := os.Getenv("GCP_E2E_ZONE"); z != "" {
		return z
	}
	return gcpE2EDefaultZone
}

func gcpE2EProvider(t *testing.T, ownerID string) *gcp.GCP {
	t.Helper()
	project, keyFile := os.Getenv("GCP_E2E_PROJECT"), os.Getenv("GCP_E2E_CREDENTIALS_FILE")
	if os.Getenv("FLEETPLANE_E2E") != "1" || project == "" || keyFile == "" {
		t.Skip("E2E gated: set FLEETPLANE_E2E=1, GCP_E2E_PROJECT and GCP_E2E_CREDENTIALS_FILE (dedicated test project only!) — ADR-015")
	}
	// The key reaches the driver only as a secret:// reference (gcp.New
	// rejects raw JSON); the test never opens it. secret://file/<p> reads
	// "/"+<p>, so an absolute path appends without an extra slash.
	abs, err := filepath.Abs(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(gcp.Settings{
		Project: project, Zone: gcpE2EZone(), CredentialsJSON: "secret://file" + abs,
	})
	g, err := gcp.New(context.Background(), provider.InstanceConfig{
		Instance: "gcp-e2e",
		OwnerID:  ownerID,
		Settings: settings,
		Secrets:  secretref.NewDefault(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestE2E_GCPConformance(t *testing.T) {
	runID := os.Getenv("GITHUB_RUN_ID")
	if runID == "" {
		runID = fmt.Sprint(time.Now().Unix())
	}
	// One owner for driver and harness: GCP Discover(ScopeOwned) filters on
	// the driver's owner, so the Discovery subtests only find what the
	// harness created when the two agree. Lowercase: GCP label values are.
	ownerID := "e2e-gcp-" + runID
	g := gcpE2EProvider(t, ownerID)
	zone := gcpE2EZone()
	conformance.Run(t, conformance.Harness{
		Provider: g,
		Kind:     g.Kind(),
		NewSpec: func(i int) json.RawMessage {
			// e2-micro = the cheapest general-purpose type; a public image
			// family (family:<project>/<family>, gcp.go image()) keeps the
			// test independent of a snapshot pipeline. Labels use the SDK
			// keys: the driver's codec maps them to fp-test / fp-test-run.
			return json.RawMessage(fmt.Sprintf(
				`{"serverType":"e2-micro","image":"family:debian-cloud/debian-12","location":%q,
				  "labels":{"fleetplane.io/test":"1","fleetplane.io/test-run":%q}}`, zone, runID))
		},
		InvalidSpec: json.RawMessage(`{"serverType":"","image":""}`),
		OwnerID:     ownerID,
		Eventual:    true,
		// The kit polls without sleeping; a pending observation is one
		// zoneOperations.get behind the driver's default pacer (5 rps, which
		// blocks rather than erroring), so 1500 steps ≈ 5+ minutes — room
		// for a GCE stop (guest shutdown alone may take ~90s) and a create.
		MaxSteps: 1500,
	})
}
