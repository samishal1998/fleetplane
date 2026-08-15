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

func e2eProvider(t *testing.T) *hetzner.Hetzner {
	t.Helper()
	if os.Getenv("FLEETPLANE_E2E") != "1" || os.Getenv("HETZNER_TOKEN") == "" {
		t.Skip("E2E gated: set FLEETPLANE_E2E=1 and HETZNER_TOKEN (dedicated test project!) — ADR-015")
	}
	settings, _ := json.Marshal(map[string]string{"token": "secret://env/HETZNER_TOKEN", "location": "fsn1"})
	h, err := hetzner.New(context.Background(), provider.InstanceConfig{
		Instance: "hetzner-e2e",
		OwnerID:  "e2e-" + fmt.Sprint(time.Now().Unix()),
		Settings: settings,
		Secrets:  secretref.NewDefault(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestE2E_HetznerConformance(t *testing.T) {
	h := e2eProvider(t)
	runID := os.Getenv("GITHUB_RUN_ID")
	if runID == "" {
		runID = fmt.Sprint(time.Now().Unix())
	}
	conformance.Run(t, conformance.Harness{
		Provider: h,
		Kind:     h.Kind(),
		NewSpec: func(i int) json.RawMessage {
			// cpx11 = the smallest x86 type; name images keep the test
			// independent of a snapshot pipeline.
			return json.RawMessage(fmt.Sprintf(
				`{"serverType":"cpx11","image":"name:ubuntu-24.04","location":"fsn1",
				  "labels":{"fleetplane.io/test":"1","fleetplane.io/test-run":%q}}`, runID))
		},
		InvalidSpec: json.RawMessage(`{"serverType":"","image":""}`),
		OwnerID:     "e2e-conformance",
		Eventual:    true,
		MaxSteps:    600, // real machines take a minute+
	})
}
