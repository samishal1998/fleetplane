package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samimishal/fleetplane/pkg/kinds/compute"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
	"github.com/samimishal/fleetplane/pkg/sdk/secretref"
)

func TestTagCodecRoundTrip(t *testing.T) {
	labels := provider.IdentityLabels("own_01ABC", "res_01DEF", "op_01GHI")
	labels[provider.LabelClass] = "ci-large"
	tags := encodeTags(labels)
	if len(tags) != 5 {
		t.Fatalf("tags = %v, want 5", tags)
	}
	for _, tag := range tags {
		if strings.ContainsAny(tag, "./=") {
			t.Fatalf("tag %q contains characters DO forbids", tag)
		}
	}
	back := decodeTags(append(tags, "user-tag", "k8s:something"))
	if !reflect.DeepEqual(back, labels) {
		t.Fatalf("round trip:\n got %v\nwant %v", back, labels)
	}
}

type doMock struct {
	mux            *http.ServeMux
	createCalls    atomic.Int64
	lastCreateBody atomic.Value
	lastTagQuery   atomic.Value
	dropletGone    atomic.Bool
}

func dropletJSON(id int, status string, tags []string) string {
	tj, _ := json.Marshal(tags)
	return fmt.Sprintf(`{"id":%d,"name":"ci-1","status":%q,"vcpus":2,"memory":4096,
		"networks":{"v4":[{"ip_address":"192.0.2.20","type":"public"}]},
		"tags":%s,"created_at":"2026-08-13T00:00:00Z"}`, id, status, tj)
}

func newDOMock(t *testing.T) (*doMock, *DigitalOcean) {
	t.Helper()
	m := &doMock{mux: http.NewServeMux()}
	ts := httptest.NewServer(m.mux)
	t.Cleanup(ts.Close)

	ourTags := []string{"fp-managed:true", "fp-owner:own_test", "fp-id:res_test", "fp-op:op_test"}

	m.mux.HandleFunc("GET /v2/droplets", func(w http.ResponseWriter, r *http.Request) {
		tag := r.URL.Query().Get("tag_name")
		m.lastTagQuery.Store(tag)
		if tag != "" && tag != "fp-op:op_test" && tag != "fp-owner:own_test" {
			fmt.Fprint(w, `{"droplets":[],"links":{},"meta":{"total":0}}`)
			return
		}
		fmt.Fprintf(w, `{"droplets":[%s],"links":{},"meta":{"total":1}}`, dropletJSON(7777, "active", ourTags))
	})
	m.mux.HandleFunc("POST /v2/droplets", func(w http.ResponseWriter, r *http.Request) {
		m.createCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		m.lastCreateBody.Store(string(body))
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"droplet":%s}`, dropletJSON(7777, "new", ourTags))
	})
	m.mux.HandleFunc("GET /v2/droplets/7777", func(w http.ResponseWriter, r *http.Request) {
		if m.dropletGone.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"id":"not_found","message":"the resource you requested could not be found"}`)
			return
		}
		fmt.Fprintf(w, `{"droplet":%s}`, dropletJSON(7777, "active", ourTags))
	})
	m.mux.HandleFunc("DELETE /v2/droplets/7777", func(w http.ResponseWriter, r *http.Request) {
		if m.dropletGone.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"id":"not_found","message":"not found"}`)
			return
		}
		m.dropletGone.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	m.mux.HandleFunc("GET /v2/images", func(w http.ResponseWriter, r *http.Request) {
		// Two snapshots with the same name: newest must win.
		fmt.Fprint(w, `{"images":[
			{"id":601,"name":"ci-runner","type":"snapshot","created_at":"2026-01-01T00:00:00Z"},
			{"id":602,"name":"ci-runner","type":"snapshot","created_at":"2026-06-01T00:00:00Z"}],
			"links":{},"meta":{"total":2}}`)
	})
	m.mux.HandleFunc("GET /v2/regions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"regions":[{"slug":"fra1"}],"links":{},"meta":{"total":1}}`)
	})

	settings, _ := json.Marshal(Settings{Token: "secret://env/DO_TEST_TOKEN", Endpoint: ts.URL + "/", Region: "fra1"})
	t.Setenv("DO_TEST_TOKEN", "test-token")
	d, err := New(context.Background(), provider.InstanceConfig{
		Instance: "do-test", OwnerID: "own_test",
		Settings: settings, Secrets: secretref.NewDefault(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, d
}

func planDOCreate(t *testing.T, d *DigitalOcean, opID string) provider.Action {
	t.Helper()
	spec, _ := json.Marshal(compute.MachineSpec{ServerType: "s-2vcpu-4gb", Image: "snapshot:ci-runner"})
	plan, err := d.Plan(context.Background(), provider.PlanRequest{
		ResourceID: "res_test",
		Desired: &provider.DesiredState{
			Name: "ci-1", Spec: spec,
			Labels: provider.IdentityLabels("own_test", "res_test", opID),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	action := plan.Actions[0]
	action.ActionID = opID
	return action
}

func TestDOCreate_TagsAndSnapshotNewest(t *testing.T) {
	m, d := newDOMock(t)
	ref, err := d.Apply(context.Background(), planDOCreate(t, d, "op_fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if m.createCalls.Load() != 1 {
		t.Fatalf("create calls = %d", m.createCalls.Load())
	}
	if ref.Ref == nil || ref.Ref.ID != "7777" {
		t.Fatalf("ref = %+v", ref.Ref)
	}
	body, _ := m.lastCreateBody.Load().(string)
	if !strings.Contains(body, `"image":602`) {
		t.Fatalf("snapshot newest-wins failed: %s", body)
	}
	for _, tag := range []string{"fp-managed:true", "fp-owner:own_test", "fp-id:res_test", "fp-op:op_fresh"} {
		if !strings.Contains(body, tag) {
			t.Fatalf("identity tag %s missing from create body: %s", tag, body)
		}
	}
}

func TestDOCreate_OpTagDedup(t *testing.T) {
	m, d := newDOMock(t)
	ref, err := d.Apply(context.Background(), planDOCreate(t, d, "op_test"))
	if err != nil {
		t.Fatal(err)
	}
	if m.createCalls.Load() != 0 {
		t.Fatalf("dedup failed: %d creates", m.createCalls.Load())
	}
	if ref.Ref.ID != "7777" {
		t.Fatalf("ref = %+v", ref.Ref)
	}
	if got, _ := m.lastTagQuery.Load().(string); got != "fp-op:op_test" {
		t.Fatalf("primary tag filter = %q", got)
	}
}

func TestDODiscover_OwnedScopeDecodesLabels(t *testing.T) {
	_, d := newDOMock(t)
	out, err := d.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeOwned})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("discovered %d droplets", len(out))
	}
	obs := out[0]
	if !obs.Owned || obs.FleetplaneID != "res_test" || obs.CreateOpID != "op_test" {
		t.Fatalf("labels not decoded from tags: %+v", obs)
	}
	if obs.Capacity[compute.DimCPU] != 2 || obs.Capacity[compute.DimMemoryMiB] != 4096 {
		t.Fatalf("capacity = %v", obs.Capacity)
	}
	if obs.Addresses[0].Addr != "192.0.2.20" || obs.Addresses[0].Network != "public-v4" {
		t.Fatalf("addresses = %v", obs.Addresses)
	}
}

func TestDOGet404IsNotFound(t *testing.T) {
	m, d := newDOMock(t)
	m.dropletGone.Store(true)
	_, err := d.Get(context.Background(), provider.ExternalRef{ID: "7777"})
	if !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDODeleteOfDeletedIsSuccess(t *testing.T) {
	m, d := newDOMock(t)
	m.dropletGone.Store(true)
	ref, err := d.Apply(context.Background(), provider.Action{
		ActionID: "op_d", Kind: "delete", Ref: &provider.ExternalRef{ID: "7777"}, Destructive: true,
	})
	if err != nil {
		t.Fatalf("delete of deleted: %v", err)
	}
	if ref.Ref.ID != "7777" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestDOObserveOperation(t *testing.T) {
	_, d := newDOMock(t)
	data, _ := json.Marshal(opData{V: 1, DropletID: 7777})
	status, err := d.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: "op_test", Ref: &provider.ExternalRef{ID: "7777"}, Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpSucceeded || status.Resource == nil {
		t.Fatalf("status = %+v", status)
	}
	if len(status.Resource.Extensions) == 0 {
		t.Fatal("extensions missing (invariant 6)")
	}
}
