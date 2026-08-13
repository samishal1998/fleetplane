package hetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"golang.org/x/time/rate"

	"github.com/samimishal/fleetplane/pkg/kinds/compute"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
	"github.com/samimishal/fleetplane/pkg/sdk/secretref"
)

// mock is a minimal Hetzner API for the calls the driver makes.
type mock struct {
	mux            *http.ServeMux
	createCalls    atomic.Int64
	lastQuery      atomic.Value // last label_selector seen on /servers
	lastCreateBody atomic.Value
	serverGone     atomic.Bool
}

func newMock(t *testing.T) (*mock, *Hetzner) {
	t.Helper()
	m := &mock{mux: http.NewServeMux()}
	ts := httptest.NewServer(m.mux)
	t.Cleanup(ts.Close)

	serverJSON := func(id int64, status string) string {
		return fmt.Sprintf(`{"id":%d,"name":"ci-1","status":%q,"created":"2026-08-13T00:00:00Z",
			"public_net":{"ipv4":{"ip":"192.0.2.10"},"ipv6":{}},
			"server_type":{"id":1,"name":"cpx31","cores":4,"memory":8.0,"architecture":"x86"},
			"labels":{"fleetplane.io/managed":"true","fleetplane.io/owner":"own_test",
			          "fleetplane.io/id":"res_test","fleetplane.io/op":"op_test"}}`, id, status)
	}

	m.mux.HandleFunc("GET /server_types", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"server_types":[{"id":1,"name":"cpx31","cores":4,"memory":8.0,"architecture":"x86"}]}`)
	})
	m.mux.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		// Two snapshots: the newer must win.
		fmt.Fprint(w, `{"images":[
			{"id":501,"type":"snapshot","status":"available","created":"2026-01-01T00:00:00Z","architecture":"x86"},
			{"id":502,"type":"snapshot","status":"available","created":"2026-06-01T00:00:00Z","architecture":"x86"}]}`)
	})
	m.mux.HandleFunc("POST /servers", func(w http.ResponseWriter, r *http.Request) {
		m.createCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		m.lastCreateBody.Store(string(body))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"server":%s,"action":{"id":9001,"status":"running","progress":0},"next_actions":[]}`,
			serverJSON(4242, "initializing"))
	})
	m.mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
		sel := r.URL.Query().Get("label_selector")
		m.lastQuery.Store(sel)
		// Server-side label filtering: our one server carries op=op_test.
		if strings.Contains(sel, "fleetplane.io/op=") && !strings.Contains(sel, "fleetplane.io/op=op_test") {
			fmt.Fprint(w, `{"servers":[]}`)
			return
		}
		fmt.Fprintf(w, `{"servers":[%s]}`, serverJSON(4242, "running"))
	})
	m.mux.HandleFunc("GET /servers/4242", func(w http.ResponseWriter, r *http.Request) {
		if m.serverGone.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"server not found"}}`)
			return
		}
		fmt.Fprintf(w, `{"server":%s}`, serverJSON(4242, "running"))
	})
	m.mux.HandleFunc("DELETE /servers/4242", func(w http.ResponseWriter, r *http.Request) {
		m.serverGone.Store(true)
		fmt.Fprint(w, `{"action":{"id":9002,"status":"success","progress":100}}`)
	})
	m.mux.HandleFunc("GET /actions/9001", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"action":{"id":9001,"status":"success","progress":100}}`)
	})
	m.mux.HandleFunc("GET /locations", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"locations":[{"id":1,"name":"fsn1"}]}`)
	})

	settings, _ := json.Marshal(Settings{Token: "secret://env/HETZNER_TEST_TOKEN", Endpoint: ts.URL})
	t.Setenv("HETZNER_TEST_TOKEN", "test-token")
	h, err := New(context.Background(), provider.InstanceConfig{
		Instance: "hetzner-test", OwnerID: "own_test",
		Settings: settings, Secrets: secretref.NewDefault(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, h
}

func planCreate(t *testing.T, h *Hetzner, opID string) provider.Action {
	t.Helper()
	spec, _ := json.Marshal(compute.MachineSpec{ServerType: "cpx31", Image: "snapshot:ci=1", Location: "fsn1"})
	plan, err := h.Plan(context.Background(), provider.PlanRequest{
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

func TestCreateFlow_SnapshotNewestWinsAndLabelsApplied(t *testing.T) {
	m, h := newMock(t)
	ref, err := h.Apply(context.Background(), planCreate(t, h, "op_fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if m.createCalls.Load() != 1 {
		t.Fatalf("create calls = %d, want 1", m.createCalls.Load())
	}
	if ref.Ref == nil || ref.Ref.ID != "4242" {
		t.Fatalf("accept-time ref missing: %+v (05 §10)", ref.Ref)
	}
	body, _ := m.lastCreateBody.Load().(string)
	if !strings.Contains(body, `"image":502`) {
		t.Fatalf("snapshot newest-wins failed; create body: %s", body)
	}
	for _, label := range []string{"fleetplane.io/managed", "fleetplane.io/owner", "fleetplane.io/id", "fleetplane.io/op"} {
		if !strings.Contains(body, label) {
			t.Fatalf("identity label %s missing from create body", label)
		}
	}
	// The dedup pre-check must have queried the composed selector.
	sel, _ := m.lastQuery.Load().(string)
	want := "fleetplane.io/managed=true,fleetplane.io/op=op_fresh,fleetplane.io/owner=own_test"
	if sel != want {
		t.Fatalf("label selector = %q, want %q", sel, want)
	}
}

// A replayed create whose op label already produced a server returns that
// server without creating (the invariant-7 anchor; conformance
// Idempotency/OpLabelDedup).
func TestCreateFlow_OpLabelDedup(t *testing.T) {
	m, h := newMock(t)
	ref, err := h.Apply(context.Background(), planCreate(t, h, "op_test"))
	if err != nil {
		t.Fatal(err)
	}
	if m.createCalls.Load() != 0 {
		t.Fatalf("dedup failed: %d create calls", m.createCalls.Load())
	}
	if ref.Ref == nil || ref.Ref.ID != "4242" {
		t.Fatalf("ref = %+v", ref.Ref)
	}
}

func TestObserveOperation_PollsActionsThenServer(t *testing.T) {
	_, h := newMock(t)
	data, _ := json.Marshal(opData{V: 1, ServerID: 4242, Actions: []int64{9001}})
	status, err := h.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: "op_test", Ref: &provider.ExternalRef{ID: "4242"}, Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpSucceeded || status.Resource == nil {
		t.Fatalf("status = %+v", status)
	}
	if status.Resource.Capacity[compute.DimCPU] != 4 || status.Resource.Capacity[compute.DimMemoryMiB] != 8192 {
		t.Fatalf("capacity = %v", status.Resource.Capacity)
	}
	if status.Resource.Addresses[0].Addr != "192.0.2.10" {
		t.Fatalf("addresses = %v", status.Resource.Addresses)
	}
	if len(status.Resource.Extensions) == 0 {
		t.Fatal("extensions missing (invariant 6)")
	}
}

func TestGet404SynthesizesNotFound(t *testing.T) {
	m, h := newMock(t)
	m.serverGone.Store(true)
	_, err := h.Get(context.Background(), provider.ExternalRef{ID: "4242"})
	if !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want synthesized ErrNotFound (hcloud returns nil,nil on 404)", err)
	}
}

func TestDeleteOfDeletedIsSuccess(t *testing.T) {
	m, h := newMock(t)
	m.serverGone.Store(true)
	ref, err := h.Apply(context.Background(), provider.Action{
		ActionID: "op_d", Kind: "delete", Ref: &provider.ExternalRef{ID: "4242"}, Destructive: true,
	})
	if err != nil {
		t.Fatalf("delete of deleted: %v", err)
	}
	if ref.Ref.ID != "4242" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestErrorMapping(t *testing.T) {
	h := &Hetzner{instance: "t", pacer: newPacer(100, 100, 5, nil)}
	cases := []struct {
		code   string
		class  provider.ErrorClass
		effect provider.SideEffect
	}{
		{"not_found", provider.ErrNotFound, provider.EffectNone},
		{"rate_limit_exceeded", provider.ErrRateLimited, provider.EffectNone},
		{"conflict", provider.ErrConflict, provider.EffectMaybe},
		{"locked", provider.ErrConflict, provider.EffectMaybe},
		{"uniqueness_error", provider.ErrConflict, provider.EffectMaybe},
		{"resource_limit_exceeded", provider.ErrQuota, provider.EffectNone},
		{"invalid_input", provider.ErrInvalid, provider.EffectNone},
		{"unauthorized", provider.ErrInvalid, provider.EffectNone},
		{"maintenance", provider.ErrRetryable, provider.EffectMaybe}, // unknown code stays retryable
	}
	for _, tc := range cases {
		err := h.mapErr(hcloudErr(tc.code), provider.EffectMaybe)
		if provider.Classify(err) != tc.class {
			t.Errorf("%s → %s, want %s", tc.code, provider.Classify(err), tc.class)
		}
		if provider.Effect(err) != tc.effect {
			t.Errorf("%s effect → %s, want %s", tc.code, provider.Effect(err), tc.effect)
		}
	}
}

func hcloudErr(code string) error {
	return fmt.Errorf("wrapped: %w", hcloud.Error{Code: hcloud.ErrorCode(code), Message: code})
}

func TestPacerPolicy(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	reset := now.Add(45 * time.Minute)
	base := rate.Limit(5)

	// Remaining 0: park until min(Reset, now+60s) — NEVER the full 45m.
	park, limit := decidePace(base, 0, reset, now)
	if park != now.Add(parkCap) || limit != 1 {
		t.Fatalf("remaining=0: park=%v limit=%v", park, limit)
	}
	// Low water: throttle to the refill rate, no park.
	park, limit = decidePace(base, 10, reset, now)
	if !park.IsZero() || limit != 1 {
		t.Fatalf("remaining=10: park=%v limit=%v", park, limit)
	}
	// Recovered: base rate restored.
	park, limit = decidePace(base, 500, reset, now)
	if !park.IsZero() || limit != base {
		t.Fatalf("remaining=500: park=%v limit=%v", park, limit)
	}

	// A parked pacer refuses calls with a RetryAfter instead of blocking.
	p := newPacer(5, 10, 5, nil)
	if d := p.on429(0); d < time.Second || d > parkCap {
		t.Fatalf("429 park duration = %v", d)
	}
	_, err := p.acquire(context.Background())
	if !provider.IsClass(err, provider.ErrRateLimited) {
		t.Fatalf("parked pacer: %v", err)
	}
	if ra, ok := provider.RetryAfterOf(err); !ok || ra <= 0 {
		t.Fatalf("no RetryAfter on parked pacer: %v %v", ra, ok)
	}
}
