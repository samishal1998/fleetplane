package docker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/pkg/sdk/conformance"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/providers/docker"
)

// testImage is tiny and has busybox sleep; override to reuse a local image.
func testImage() string {
	if img := os.Getenv("FLEETPLANE_DOCKER_IMAGE"); img != "" {
		return img
	}
	return "alpine:3.20"
}

// newDocker returns a driver bound to a fresh owner. Skips when no daemon is
// reachable unless FLEETPLANE_E2E_DOCKER=1 turns the skip into a failure
// (CI must never silently pass by skipping). Every container created here
// carries LabelTest and is swept at cleanup.
func newDocker(t *testing.T) (*docker.Docker, string) {
	t.Helper()
	owner := fmt.Sprintf("docker-test-%d", time.Now().UnixNano())
	d, err := docker.New(provider.InstanceConfig{Instance: "docker-test", OwnerID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Health(context.Background()); err != nil {
		if os.Getenv("FLEETPLANE_E2E_DOCKER") == "1" {
			t.Fatalf("FLEETPLANE_E2E_DOCKER=1 but docker unreachable: %v", err)
		}
		t.Skipf("docker unreachable, skipping: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		list, err := d.Discover(ctx, provider.DiscoverRequest{Scope: provider.ScopeOwned, Selector: map[string]string{provider.LabelTest: "1"}})
		if err != nil {
			t.Logf("sweep: %v", err)
			return
		}
		for _, c := range list {
			_, _ = d.Apply(ctx, provider.Action{ActionID: "sweep", Kind: "delete", Ref: &c.Ref, Destructive: true})
		}
	})
	return d, owner
}

func spec() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"serverType":"1x64","image":"name:%s","labels":{"fleetplane.io/test":"1"}}`, testImage()))
}

func TestDockerConformance(t *testing.T) {
	d, owner := newDocker(t)
	conformance.Run(t, conformance.Harness{
		Provider:    d,
		Kind:        d.Kind(),
		NewSpec:     func(int) json.RawMessage { return spec() },
		InvalidSpec: json.RawMessage(`{"serverType":"cpx11","image":"snapshot:x=1"}`),
		OwnerID:     owner,
		MaxSteps:    300,
	})
}

// A crash between `create` and `start` leaves a labeled container in the
// `created` state; the create op must adopt and start it, never make a
// second container.
func TestDockerAdoptsCreatedNotStarted(t *testing.T) {
	d, owner := newDocker(t)
	ctx := context.Background()
	resID, opID := "res-adopt", fmt.Sprintf("op-adopt-%d", time.Now().UnixNano())
	plan, err := d.Plan(ctx, provider.PlanRequest{ResourceID: resID, Desired: &provider.DesiredState{
		Name: "adopt", Spec: spec(), Labels: provider.IdentityLabels(owner, resID, opID),
	}})
	if err != nil {
		t.Fatal(err)
	}
	action := plan.Actions[0]
	action.ActionID = opID

	// Seed the half-done create straight through the socket: same labels, no start.
	labels := provider.IdentityLabels(owner, resID, opID)
	labels[provider.LabelTest] = "1"
	body, _ := json.Marshal(map[string]any{"Image": testImage(), "Cmd": []string{"sleep", "60"}, "Labels": labels})
	sock := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dl net.Dialer
		return dl.DialContext(ctx, "unix", "/var/run/docker.sock")
	}}}
	// The seed bypasses the driver, so it must pull the image itself: on a
	// fresh runner with -shuffle this test may run before anything else has.
	pull, err := sock.Post("http://docker/images/create?fromImage="+testImage(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, pull.Body)
	_ = pull.Body.Close()
	res, err := sock.Post("http://docker/containers/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatalf("seed create: %d", res.StatusCode)
	}

	opRef, err := d.Apply(ctx, action)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		st, err := d.ObserveOperation(ctx, opRef)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == provider.OpSucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	list, err := d.Discover(ctx, provider.DiscoverRequest{Scope: provider.ScopeOwned, Selector: map[string]string{provider.LabelOp: opID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Phase != provider.PhaseRunning {
		t.Fatalf("want exactly one running adopted container, got %+v", list)
	}
}

// Plan rejects bad sizes/images as ErrInvalid without touching the daemon.
func TestDockerPlanValidation(t *testing.T) {
	d, _ := docker.New(provider.InstanceConfig{Instance: "x"})
	for _, raw := range []string{
		`{"serverType":"","image":"alpine"}`,
		`{"serverType":"x","image":"alpine"}`,
		`{"serverType":"0x64","image":"alpine"}`,
		`{"serverType":"2x1","image":"alpine"}`,
		`{"serverType":"cpx11","image":"alpine"}`,
		`{"serverType":"1","image":"snapshot:x=1"}`,
	} {
		_, err := d.Plan(context.Background(), provider.PlanRequest{ResourceID: "r", Desired: &provider.DesiredState{Spec: json.RawMessage(raw)}})
		if !provider.IsClass(err, provider.ErrInvalid) || provider.Effect(err) != provider.EffectNone {
			t.Fatalf("%s: err=%v, want invalid/none", raw, err)
		}
	}
	if _, err := d.Plan(context.Background(), provider.PlanRequest{ResourceID: "r", Desired: &provider.DesiredState{
		Spec: json.RawMessage(`{"serverType":"2","image":"name:alpine:3.20"}`)}}); err != nil {
		t.Fatalf("bare cpu size must be valid: %v", err)
	}
}
