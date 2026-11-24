package tests

// Env-gated E2E against a REAL Hetzner project (ADR-015): double gate
// (FLEETPLANE_E2E=1 ∧ HETZNER_TOKEN), dedicated throwaway project only,
// every resource labeled fleetplane.io/test, cleanup guaranteed by
// t.Cleanup plus the CI sweeper.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/pkg/sdk/conformance"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
	"github.com/samishal1998/fleetplane/providers/hetzner"
)

// e2eProvider builds the driver with ownerID. It must be the same ID the
// conformance kit labels its resources with: owned-scope discovery returns
// only resources whose owner label matches the driver's.
func e2eProvider(t *testing.T, ownerID string) *hetzner.Hetzner {
	t.Helper()
	if os.Getenv("FLEETPLANE_E2E") != "1" || os.Getenv("HETZNER_TOKEN") == "" {
		t.Skip("E2E gated: set FLEETPLANE_E2E=1 and HETZNER_TOKEN (dedicated test project!) — ADR-015")
	}
	settings, _ := json.Marshal(map[string]string{"token": "secret://env/HETZNER_TOKEN", "location": e2eHetznerLocation()})
	h, err := hetzner.New(context.Background(), provider.InstanceConfig{
		Instance: "hetzner-e2e",
		OwnerID:  ownerID,
		Settings: settings,
		Secrets:  secretref.NewDefault(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestE2E_HetznerConformance(t *testing.T) {
	runID := os.Getenv("GITHUB_RUN_ID")
	if runID == "" {
		runID = fmt.Sprint(time.Now().Unix())
	}
	owner := "e2e-hetzner-" + runID
	h := e2eProvider(t, owner)
	conformance.Run(t, conformance.Harness{
		Provider: h,
		Kind:     h.Kind(),
		NewSpec: func(i int) json.RawMessage {
			// Name images keep the test independent of a snapshot pipeline.
			return json.RawMessage(fmt.Sprintf(
				`{"serverType":%q,"image":"name:ubuntu-24.04","location":%q,
				  "labels":{"fleetplane.io/test":"1","fleetplane.io/test-run":%q}}`,
				envOr("HETZNER_E2E_SERVER_TYPE", "cpx22"), e2eHetznerLocation(), runID))
		},
		InvalidSpec: json.RawMessage(`{"serverType":"","image":""}`),
		OwnerID:     owner,
		// Well-formed but unassigned: exercises the driver's synthesis of
		// a real 404 into ErrNotFound (hcloud's Get returns nil, nil).
		MissingID: "9000000000000000000",
		Eventual:  true,
		MaxSteps:  600, // real machines take a minute+
	})
}

// Hetzner retires server types per location (cpx11 stopped being orderable
// in the EU locations after 2025-12-31), so the type and location are
// overridable without a code change. cpx22 is the default because the
// cheaper cx23 is often sold out across all EU locations (it was on 2026-11-23).
func e2eHetznerLocation() string { return envOr("HETZNER_E2E_LOCATION", "fsn1") }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
