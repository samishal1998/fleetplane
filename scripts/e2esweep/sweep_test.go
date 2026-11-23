package main

// Offline checks for the GCP sweep's safety rule (ADR-015), against an
// httptest Compute mock that deliberately IGNORES the label filter: the
// client-side re-check alone must keep unlabeled instances alive.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	gce "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/providers/gcp"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// The sweeper hardcodes the driver's encoding of fleetplane.io/test; prove it
// matches what the driver actually writes on create.
func TestGCPTestKeyMatchesDriverEncoding(t *testing.T) {
	var mu sync.Mutex
	var got map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /projects/p/zones/z/instances", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &gce.InstanceList{}) // create-dedup lookup: nothing yet
	})
	mux.HandleFunc("POST /projects/p/zones/z/instances", func(w http.ResponseWriter, r *http.Request) {
		var inst gce.Instance
		_ = json.NewDecoder(r.Body).Decode(&inst)
		mu.Lock()
		got = inst.Labels
		mu.Unlock()
		writeJSON(w, &gce.Operation{Name: "op-1"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	settings, _ := json.Marshal(gcp.Settings{Project: "p", Zone: "z", Endpoint: ts.URL})
	g, err := gcp.New(context.Background(), provider.InstanceConfig{Instance: "gcp", OwnerID: "o", Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := g.Plan(context.Background(), provider.PlanRequest{ResourceID: "r", Desired: &provider.DesiredState{
		Name: "x", Spec: json.RawMessage(`{"serverType":"e2-micro","image":"id:img","labels":{"fleetplane.io/test":"1"}}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	action := plan.Actions[0]
	action.ActionID = "op-a"
	if _, err := g.Apply(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got[gcpTestKey] != "1" {
		t.Fatalf("driver wrote labels %v; sweeper looks for %s=1", got, gcpTestKey)
	}
}

func TestSweepGCPDeletesOnlyOldTestLabeled(t *testing.T) {
	old := time.Now().Add(-3 * time.Hour).Format(time.RFC3339)
	young := time.Now().Format(time.RFC3339)
	zoneURL := "https://compute.googleapis.com/compute/v1/projects/p/zones/"
	instances := []*gce.Instance{
		{Name: "old-test", Zone: zoneURL + "us-central1-a", CreationTimestamp: old, Labels: map[string]string{"fp-test": "1"}},
		{Name: "young-test", Zone: zoneURL + "europe-west1-b", CreationTimestamp: young, Labels: map[string]string{"fp-test": "1"}},
		{Name: "old-prod", Zone: zoneURL + "us-central1-a", CreationTimestamp: old, Labels: map[string]string{"app": "prod"}},
		{Name: "old-test-0", Zone: zoneURL + "us-central1-a", CreationTimestamp: old, Labels: map[string]string{"fp-test": "0"}},
		{Name: "dotted-key", Zone: zoneURL + "us-central1-a", CreationTimestamp: old, Labels: map[string]string{"fleetplane-io-test": "1"}},
	}

	for _, tc := range []struct {
		name   string
		maxAge time.Duration
		want   []string
	}{
		{"backstop", 2 * time.Hour, []string{"us-central1-a/old-test"}},
		{"sweep-all", 0, []string{"europe-west1-b/young-test", "us-central1-a/old-test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var deleted []string
			mux := http.NewServeMux()
			mux.HandleFunc("GET /projects/p/aggregated/instances", func(w http.ResponseWriter, r *http.Request) {
				if f := r.URL.Query().Get("filter"); f != `labels.fp-test = "1"` {
					t.Errorf("server-side filter = %q, want the test label", f)
				}
				writeJSON(w, &gce.InstanceAggregatedList{Items: map[string]gce.InstancesScopedList{
					"zones/all": {Instances: instances}, // filter ignored on purpose
				}})
			})
			mux.HandleFunc("DELETE /projects/p/zones/{zone}/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				deleted = append(deleted, r.PathValue("zone")+"/"+r.PathValue("name"))
				mu.Unlock()
				writeJSON(w, &gce.Operation{Name: "op"})
			})
			ts := httptest.NewServer(mux)
			t.Cleanup(ts.Close)

			svc, err := gce.NewService(context.Background(), option.WithEndpoint(ts.URL), option.WithoutAuthentication())
			if err != nil {
				t.Fatal(err)
			}
			if !sweepGCP(context.Background(), svc, "p", tc.maxAge) {
				t.Fatal("sweepGCP reported failure")
			}
			mu.Lock()
			defer mu.Unlock()
			slices.Sort(deleted)
			if !slices.Equal(deleted, tc.want) {
				t.Fatalf("deleted %v, want %v", deleted, tc.want)
			}
		})
	}
}
