package fake_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/samishal1998/fleetplane/pkg/sdk/conformance"
	"github.com/samishal1998/fleetplane/providers/fake"
)

// The fake provider passes the full conformance catalog under its most
// hostile deterministic settings: multi-step async creates/deletes, list
// lag, and internal pagination (Phase-2 exit).
func TestFakeConformance(t *testing.T) {
	f := fake.New("fake-conf", "owner-conf", fake.Options{
		CreateSteps:  2,
		DeleteSteps:  1,
		ListLagSteps: 2,
		PageSize:     2,
		Park:         true,
		StopSteps:    2,
		StartSteps:   2,
	})
	conformance.Run(t, conformance.Harness{
		Provider: f,
		Kind:     f.Kind(),
		NewSpec: func(i int) json.RawMessage {
			return json.RawMessage(fmt.Sprintf(`{"serverType":"cpx%d1","image":"snapshot:ci=1"}`, i%9+1))
		},
		InvalidSpec: json.RawMessage(`{"serverType":""}`),
		OwnerID:     "owner-conf",
		Eventual:    true,
		Expensive:   true,
	})
}

// Synchronous mode passes identically (near-miss: the kit must not depend
// on asynchrony).
func TestFakeConformanceSynchronous(t *testing.T) {
	f := fake.New("fake-conf-sync", "owner-conf", fake.Options{Park: true})
	conformance.Run(t, conformance.Harness{
		Provider: f,
		Kind:     f.Kind(),
		NewSpec: func(int) json.RawMessage {
			return json.RawMessage(`{"serverType":"cpx31","image":"snapshot:ci=1"}`)
		},
		InvalidSpec: json.RawMessage(`{}`),
		OwnerID:     "owner-conf",
	})
}
