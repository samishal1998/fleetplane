package gcp

// The GCP driver runs the shared conformance catalog (docs/03 §8) — the
// Parking subtests included — against a minimal STATEFUL Compute Engine
// mock: instances CRUD, stop/start power transitions (with a fresh
// ephemeral IP on every start, like GCE), zonal operations, and the
// aggregated/zonal lists the driver's Discover and create-dedup use.
// Filters are deliberately NOT honored server-side: the driver's
// client-side re-checks must carry the contract alone.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	gce "google.golang.org/api/compute/v1"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/conformance"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

type confServer struct {
	mu        sync.Mutex
	instances map[string]*confInstance
	ipSeq     int
	opSeq     int
}

type confInstance struct {
	name   string
	status string
	labels map[string]string
	mt     string
	natIP  string
}

const confZoneURL = "https://compute.googleapis.com/compute/v1/projects/" + testProject + "/zones/" + testZone

func (s *confServer) toGCE(ci *confInstance) *gce.Instance {
	ni := &gce.NetworkInterface{NetworkIP: "10.0.0.9"}
	if ci.natIP != "" {
		ni.AccessConfigs = []*gce.AccessConfig{{Type: "ONE_TO_ONE_NAT", NatIP: ci.natIP}}
	}
	return &gce.Instance{
		Name:              ci.name,
		Status:            ci.status,
		Zone:              confZoneURL,
		MachineType:       confZoneURL + "/machineTypes/" + ci.mt,
		Labels:            ci.labels,
		NetworkInterfaces: []*gce.NetworkInterface{ni},
		CreationTimestamp: "2026-08-15T00:00:00Z",
	}
}

func (s *confServer) doneOp(kind string) *gce.Operation {
	s.opSeq++
	return &gce.Operation{Name: fmt.Sprintf("op-conf-%d", s.opSeq), Status: "DONE", OperationType: kind}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *confServer) freshIP() string {
	s.ipSeq++
	return fmt.Sprintf("198.51.100.%d", s.ipSeq%200+1)
}

func newConfServer() *http.ServeMux {
	s := &confServer{instances: map[string]*confInstance{}}
	mux := http.NewServeMux()
	base := "/projects/" + testProject
	zonal := base + "/zones/" + testZone

	mux.HandleFunc("POST "+zonal+"/instances", func(w http.ResponseWriter, r *http.Request) {
		var inst gce.Instance
		if err := json.NewDecoder(r.Body).Decode(&inst); err != nil || inst.Name == "" {
			gceErr(w, http.StatusBadRequest, "badRequest", "malformed instance")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.instances[inst.Name]; ok {
			gceErr(w, http.StatusConflict, "alreadyExists", "instance "+inst.Name+" already exists")
			return
		}
		s.instances[inst.Name] = &confInstance{
			name: inst.Name, status: "RUNNING", labels: inst.Labels,
			mt: lastSegment(inst.MachineType), natIP: s.freshIP(),
		}
		writeJSON(w, s.doneOp("insert"))
	})
	list := func() []*gce.Instance {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []*gce.Instance
		for _, ci := range s.instances {
			out = append(out, s.toGCE(ci))
		}
		return out
	}
	mux.HandleFunc("GET "+zonal+"/instances", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, &gce.InstanceList{Items: list()})
	})
	mux.HandleFunc("GET "+base+"/aggregated/instances", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, &gce.InstanceAggregatedList{Items: map[string]gce.InstancesScopedList{
			"zones/" + testZone: {Instances: list()},
		}})
	})
	withInstance := func(w http.ResponseWriter, r *http.Request, fn func(*confInstance)) {
		s.mu.Lock()
		defer s.mu.Unlock()
		ci, ok := s.instances[r.PathValue("name")]
		if !ok {
			gceErr(w, http.StatusNotFound, "notFound", "instance "+r.PathValue("name")+" was not found")
			return
		}
		fn(ci)
	}
	mux.HandleFunc("GET "+zonal+"/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		withInstance(w, r, func(ci *confInstance) { writeJSON(w, s.toGCE(ci)) })
	})
	mux.HandleFunc("DELETE "+zonal+"/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		withInstance(w, r, func(ci *confInstance) {
			delete(s.instances, ci.name)
			writeJSON(w, s.doneOp("delete"))
		})
	})
	mux.HandleFunc("POST "+zonal+"/instances/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		withInstance(w, r, func(ci *confInstance) {
			ci.status, ci.natIP = "TERMINATED", "" // the ephemeral IP is RELEASED
			writeJSON(w, s.doneOp("stop"))
		})
	})
	mux.HandleFunc("POST "+zonal+"/instances/{name}/start", func(w http.ResponseWriter, r *http.Request) {
		withInstance(w, r, func(ci *confInstance) {
			ci.status, ci.natIP = "RUNNING", s.freshIP() // a NEW ephemeral IP
			writeJSON(w, s.doneOp("start"))
		})
	})
	mux.HandleFunc("GET "+zonal+"/operations/{op}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, &gce.Operation{Name: r.PathValue("op"), Status: "DONE"})
	})
	mux.HandleFunc("GET "+zonal+"/machineTypes/{mt}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, &gce.MachineType{Name: r.PathValue("mt"), GuestCpus: 2, MemoryMb: 4096})
	})
	return mux
}

func TestGCPConformance(t *testing.T) {
	ts := httptest.NewServer(newConfServer())
	t.Cleanup(ts.Close)

	// High RPS: the pacer still wraps every call (same code path), but the
	// suite is not slowed to the production default rate against a mock.
	settings, _ := json.Marshal(Settings{
		Project: testProject, Zone: testZone, Endpoint: ts.URL, RPS: 5000, Burst: 5000,
	})
	g, err := New(context.Background(), provider.InstanceConfig{
		Instance: "gcp-conf", OwnerID: "owner-conf", Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	conformance.Run(t, conformance.Harness{
		Provider: g,
		Kind:     compute.Kind,
		NewSpec: func(i int) json.RawMessage {
			// "id:" image form: resolved without an API round trip.
			return json.RawMessage(fmt.Sprintf(`{"serverType":"e2-medium","image":"id:img-%d"}`, i))
		},
		InvalidSpec: json.RawMessage(`{"serverType":"e2-medium"}`), // no image
		OwnerID:     "owner-conf",
		Expensive:   true,
	})
}
