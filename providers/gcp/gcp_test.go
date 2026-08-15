package gcp

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
	"time"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

// Test identities use uppercase ULID-ish payloads so the codec's
// lowercase-encode / uppercase-restore is actually exercised.
const (
	testProject = "test-proj"
	testZone    = "us-central1-a"
	testOwner   = "own_01OWNER"
	testResID   = "res_01ABC"
	takenOpID   = "op_01TAKEN" // the op that already produced instance ci-1
)

func TestGCPLabelCodecRoundTrip(t *testing.T) {
	labels := provider.IdentityLabels(testOwner, testResID, "op_01FRESH")
	labels[provider.LabelClass] = "ci-large"
	labels[provider.LabelTest] = "true"
	labels[provider.LabelTestRun] = "tr_01RUN"

	enc := encodeLabels(labels)
	if len(enc) != len(labels) {
		t.Fatalf("encoded %d labels, want %d: %v", len(enc), len(labels), enc)
	}
	for k, v := range enc {
		for _, s := range []string{k, v} {
			if s != strings.ToLower(s) || strings.ContainsAny(s, "./ ") {
				t.Fatalf("encoded label %q=%q violates the GCP charset", k, v)
			}
		}
		if k[0] < 'a' || k[0] > 'z' {
			t.Fatalf("encoded key %q must start with a letter", k)
		}
	}
	if enc["fp-op"] != "op_01fresh" || enc["fp-owner"] != "own_01owner" {
		t.Fatalf("identity values not lowercased: %v", enc)
	}

	back := decodeLabels(enc)
	if !reflect.DeepEqual(back, labels) {
		t.Fatalf("round trip:\n got %v\nwant %v", back, labels)
	}
}

func TestGCPLabelCodecSanitize(t *testing.T) {
	if got := encodeKey("Env/Stage.Name"); got != "env-stage-name" {
		t.Fatalf("encodeKey sanitize = %q", got)
	}
	if got := encodeKey("9lives"); got != "u-9lives" {
		t.Fatalf("encodeKey non-letter start = %q", got)
	}
	if got := encodeValue("CI-Runner v2"); got != "ci-runner-v2" {
		t.Fatalf("encodeValue = %q", got)
	}
	// Decode passes foreign keys through verbatim.
	back := decodeLabels(map[string]string{"goog-terraform": "x", "fp-class": "ci-large"})
	if back["goog-terraform"] != "x" || back[provider.LabelClass] != "ci-large" {
		t.Fatalf("decode = %v", back)
	}
}

// --- mock GCE API ---

type gceMock struct {
	mux          *http.ServeMux
	insertCalls  atomic.Int64
	stopCalls    atomic.Int64
	startCalls   atomic.Int64
	lastInsert   atomic.Value // string: request body
	lastFilter   atomic.Value // string: instances list filter
	lastListPath atomic.Value // string: URL path of the last instances list
	listStatus   atomic.Value // string: status the listed instance reports
	instStatus   atomic.Value // string: status instances.get reports for ci-1
	natIP        atomic.Value // string: ci-1's ephemeral external IP
	instanceGone atomic.Bool
	get429       atomic.Bool
	stop400      atomic.Bool  // stop rejects 400 (local SSD without discardLocalSsd)
	start400     atomic.Bool  // start rejects 400
	raceTo       atomic.Value // string: status instances.get reports AFTER a 400-rejected stop/start (idempotency race)
	opStatus     atomic.Value // string: zonal operation status
	opError      atomic.Bool
}

func gceErr(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"errors":[{"reason":%q,"message":%q}]}}`,
		code, msg, reason, msg)
}

func instanceJSON(status, natIP string) string {
	return fmt.Sprintf(`{
		"name":"ci-1","status":%q,
		"zone":"https://compute.googleapis.com/compute/v1/projects/test-proj/zones/us-central1-a",
		"machineType":"https://compute.googleapis.com/compute/v1/projects/test-proj/zones/us-central1-a/machineTypes/e2-medium",
		"labels":{"fp-managed":"true","fp-owner":"own_01owner","fp-id":"res_01abc","fp-op":"op_01taken"},
		"networkInterfaces":[{"networkIP":"10.0.0.2","accessConfigs":[{"type":"ONE_TO_ONE_NAT","natIP":%q}]}],
		"creationTimestamp":"2026-08-13T00:00:00Z"}`, status, natIP)
}

func newGCEMock(t *testing.T) (*gceMock, *GCP) {
	t.Helper()
	m := &gceMock{mux: http.NewServeMux()}
	m.opStatus.Store("DONE")
	m.listStatus.Store("RUNNING")
	m.instStatus.Store("RUNNING")
	m.natIP.Store("203.0.113.5")
	ts := httptest.NewServer(m.mux)
	t.Cleanup(ts.Close)

	base := "/projects/" + testProject
	zonal := base + "/zones/" + testZone

	m.mux.HandleFunc("POST "+zonal+"/instances", func(w http.ResponseWriter, r *http.Request) {
		m.insertCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		m.lastInsert.Store(string(body))
		fmt.Fprint(w, `{"name":"op-ins-1","status":"RUNNING","operationType":"insert"}`)
	})
	// The mock serves ci-1 from BOTH list endpoints (unless gone / the fp-op
	// filter targets another op): the zonal list backs create-dedup, the
	// aggregated list backs Discover. Selector filters beyond fp-op are
	// deliberately NOT honored server-side, proving the client-side re-check.
	listInstances := func(w http.ResponseWriter, r *http.Request, wrap string) {
		m.lastListPath.Store(r.URL.Path)
		filter := r.URL.Query().Get("filter")
		m.lastFilter.Store(filter)
		if m.instanceGone.Load() ||
			(strings.Contains(filter, "labels.fp-op") && !strings.Contains(filter, `"op_01taken"`)) {
			fmt.Fprintf(w, wrap, "")
			return
		}
		fmt.Fprintf(w, wrap, instanceJSON(m.listStatus.Load().(string), "203.0.113.5"))
	}
	m.mux.HandleFunc("GET "+zonal+"/instances", func(w http.ResponseWriter, r *http.Request) {
		listInstances(w, r, `{"items":[%s]}`)
	})
	m.mux.HandleFunc("GET "+base+"/aggregated/instances", func(w http.ResponseWriter, r *http.Request) {
		listInstances(w, r, `{"items":{"zones/us-central1-a":{"instances":[%s]}}}`)
	})
	m.mux.HandleFunc("GET "+zonal+"/instances/ci-1", func(w http.ResponseWriter, r *http.Request) {
		if m.get429.Load() {
			w.Header().Set("Retry-After", "7")
			gceErr(w, http.StatusTooManyRequests, "rateLimitExceeded", "rate limit exceeded")
			return
		}
		if m.instanceGone.Load() {
			gceErr(w, http.StatusNotFound, "notFound", "instance ci-1 was not found")
			return
		}
		fmt.Fprint(w, instanceJSON(m.instStatus.Load().(string), m.natIP.Load().(string)))
	})
	m.mux.HandleFunc("POST "+zonal+"/instances/ci-1/stop", func(w http.ResponseWriter, r *http.Request) {
		if m.instanceGone.Load() {
			gceErr(w, http.StatusNotFound, "notFound", "instance ci-1 was not found")
			return
		}
		if m.stop400.Load() {
			if to, _ := m.raceTo.Load().(string); to != "" {
				m.instStatus.Store(to) // the race: another actor already moved it
			}
			gceErr(w, http.StatusBadRequest, "badRequest",
				"Stopping a VM with a Local SSD attached requires setting discardLocalSsd")
			return
		}
		m.stopCalls.Add(1)
		m.instStatus.Store("STOPPING")
		fmt.Fprint(w, `{"name":"op-stop-1","status":"RUNNING","operationType":"stop"}`)
	})
	m.mux.HandleFunc("POST "+zonal+"/instances/ci-1/start", func(w http.ResponseWriter, r *http.Request) {
		if m.instanceGone.Load() {
			gceErr(w, http.StatusNotFound, "notFound", "instance ci-1 was not found")
			return
		}
		if m.start400.Load() {
			if to, _ := m.raceTo.Load().(string); to != "" {
				m.instStatus.Store(to)
			}
			gceErr(w, http.StatusBadRequest, "badRequest", "Instance ci-1 is not in TERMINATED state")
			return
		}
		m.startCalls.Add(1)
		m.instStatus.Store("STAGING")
		m.natIP.Store("203.0.113.99") // GCE hands out a NEW ephemeral IP on start
		fmt.Fprint(w, `{"name":"op-start-1","status":"RUNNING","operationType":"start"}`)
	})
	m.mux.HandleFunc("DELETE "+zonal+"/instances/ci-1", func(w http.ResponseWriter, r *http.Request) {
		if m.instanceGone.Load() {
			gceErr(w, http.StatusNotFound, "notFound", "instance ci-1 was not found")
			return
		}
		m.instanceGone.Store(true)
		fmt.Fprint(w, `{"name":"op-del-1","status":"RUNNING","operationType":"delete"}`)
	})
	m.mux.HandleFunc("GET "+zonal+"/operations/op-ins-1", func(w http.ResponseWriter, r *http.Request) {
		if m.opError.Load() {
			fmt.Fprint(w, `{"name":"op-ins-1","status":"DONE","operationType":"insert",
				"error":{"errors":[{"code":"QUOTA_EXCEEDED","message":"quota 'CPUS' exceeded"}]}}`)
			return
		}
		fmt.Fprintf(w, `{"name":"op-ins-1","status":%q,"operationType":"insert"}`, m.opStatus.Load())
	})
	m.mux.HandleFunc("GET "+zonal+"/operations/op-stop-1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"name":"op-stop-1","status":%q,"operationType":"stop"}`, m.opStatus.Load())
	})
	m.mux.HandleFunc("GET "+zonal+"/operations/op-start-1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"name":"op-start-1","status":%q,"operationType":"start"}`, m.opStatus.Load())
	})
	m.mux.HandleFunc("GET "+zonal+"/machineTypes/e2-medium", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"e2-medium","guestCpus":2,"memoryMb":4096}`)
	})
	m.mux.HandleFunc("GET "+zonal, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"us-central1-a","status":"UP"}`)
	})
	m.mux.HandleFunc("GET "+base+"/global/images", func(w http.ResponseWriter, r *http.Request) {
		// Two snapshots carrying the same encoded label: newest must win.
		fmt.Fprint(w, `{"items":[
			{"name":"img-old","selfLink":"https://compute.googleapis.com/compute/v1/projects/test-proj/global/images/img-old",
			 "labels":{"build":"ci-runner"},"creationTimestamp":"2026-01-01T00:00:00Z"},
			{"name":"img-new","selfLink":"https://compute.googleapis.com/compute/v1/projects/test-proj/global/images/img-new",
			 "labels":{"build":"ci-runner"},"creationTimestamp":"2026-06-01T00:00:00Z"},
			{"name":"unrelated","selfLink":"https://compute.googleapis.com/compute/v1/projects/test-proj/global/images/unrelated",
			 "labels":{"build":"other"},"creationTimestamp":"2026-07-01T00:00:00Z"}]}`)
	})
	m.mux.HandleFunc("GET "+base+"/global/images/family/debian-12", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"debian-12-local","selfLink":"https://compute.googleapis.com/compute/v1/projects/test-proj/global/images/debian-12-local"}`)
	})
	m.mux.HandleFunc("GET /projects/debian-cloud/global/images/family/debian-12", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"debian-12-v20260801","selfLink":"https://compute.googleapis.com/compute/v1/projects/debian-cloud/global/images/debian-12-v20260801"}`)
	})
	m.mux.HandleFunc("GET "+base+"/global/images/base-img", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"base-img","selfLink":"https://compute.googleapis.com/compute/v1/projects/test-proj/global/images/base-img"}`)
	})
	m.mux.HandleFunc("GET "+base+"/global/images/missing-img", func(w http.ResponseWriter, r *http.Request) {
		gceErr(w, http.StatusNotFound, "notFound", "image missing-img was not found")
	})

	settings, _ := json.Marshal(Settings{Project: testProject, Zone: testZone, Endpoint: ts.URL})
	g, err := New(context.Background(), provider.InstanceConfig{
		Instance: "gcp-test", OwnerID: testOwner, Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, g
}

func planCreate(t *testing.T, g *GCP, opID string) provider.Action {
	t.Helper()
	spec, _ := json.Marshal(compute.MachineSpec{
		ServerType: "e2-medium",
		Image:      "snapshot:Build=CI-Runner", // encoded client-side to build=ci-runner
		UserData:   "#cloud-config\nruncmd: [echo hi]\n",
	})
	plan, err := g.Plan(context.Background(), provider.PlanRequest{
		ResourceID: testResID,
		Desired: &provider.DesiredState{
			Name: "ci-1", Spec: spec,
			Labels: provider.IdentityLabels(testOwner, testResID, opID),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	action := plan.Actions[0]
	action.ActionID = opID
	return action
}

// --- settings ---

func TestGCPSettingsValidation(t *testing.T) {
	for name, settings := range map[string]Settings{
		"missing project": {Zone: testZone},
		"missing zone":    {Project: testProject},
		"raw credentials": {Project: testProject, Zone: testZone, CredentialsJSON: `{"type":"service_account"}`},
	} {
		raw, _ := json.Marshal(settings)
		_, err := New(context.Background(), provider.InstanceConfig{Instance: "gcp-bad", Settings: raw})
		if !provider.IsClass(err, provider.ErrInvalid) || provider.Effect(err) != provider.EffectNone {
			t.Fatalf("%s: err = %v, want ErrInvalid/EffectNone", name, err)
		}
	}
}

// --- create ---

func TestGCPCreate_LabelsSnapshotNewestAndShape(t *testing.T) {
	m, g := newGCEMock(t)
	ref, err := g.Apply(context.Background(), planCreate(t, g, "op_01FRESH"))
	if err != nil {
		t.Fatal(err)
	}
	if m.insertCalls.Load() != 1 {
		t.Fatalf("insert calls = %d", m.insertCalls.Load())
	}
	if ref.Ref == nil || ref.Ref.ID != "ci-1" || ref.Ref.Extra["zone"] != testZone {
		t.Fatalf("ref = %+v", ref.Ref)
	}
	var data opData
	if err := json.Unmarshal(ref.Data, &data); err != nil || data.Op != "op-ins-1" || data.Zone != testZone || data.Instance != "ci-1" {
		t.Fatalf("op data = %s (err %v)", ref.Data, err)
	}

	body, _ := m.lastInsert.Load().(string)
	// Snapshot resolution: encoded-label match, newest creationTimestamp.
	if !strings.Contains(body, "/global/images/img-new") || strings.Contains(body, "img-old") {
		t.Fatalf("snapshot newest-wins failed: %s", body)
	}
	for _, frag := range []string{
		`"fp-managed":"true"`, `"fp-owner":"own_01owner"`, `"fp-id":"res_01abc"`, `"fp-op":"op_01fresh"`,
		`"machineType":"zones/us-central1-a/machineTypes/e2-medium"`,
		`"network":"global/networks/default"`, `"type":"ONE_TO_ONE_NAT"`,
		`"key":"user-data"`, `"boot":true`, `"autoDelete":true`,
	} {
		if !strings.Contains(body, frag) {
			t.Fatalf("create body missing %s: %s", frag, body)
		}
	}
}

func TestGCPCreate_OpLabelDedup(t *testing.T) {
	m, g := newGCEMock(t)
	ref, err := g.Apply(context.Background(), planCreate(t, g, takenOpID))
	if err != nil {
		t.Fatal(err)
	}
	if m.insertCalls.Load() != 0 {
		t.Fatalf("dedup failed: %d inserts", m.insertCalls.Load())
	}
	if ref.Ref == nil || ref.Ref.ID != "ci-1" {
		t.Fatalf("ref = %+v", ref.Ref)
	}
	if got, _ := m.lastFilter.Load().(string); got != `labels.fp-op = "op_01taken"` {
		t.Fatalf("dedup filter = %q", got)
	}
	if got, _ := m.lastListPath.Load().(string); !strings.HasSuffix(got, "/zones/"+testZone+"/instances") {
		t.Fatalf("dedup must list the deterministic create zone, listed %q", got)
	}
}

func TestGCPCreate_DedupSkipsTerminatedLeftover(t *testing.T) {
	// A TERMINATED (mid-deletion) leftover of the SAME op is never reused:
	// the create proceeds (a still-live name then 409s — never a second
	// instance under a different name).
	m, g := newGCEMock(t)
	m.listStatus.Store("TERMINATED")
	if _, err := g.Apply(context.Background(), planCreate(t, g, takenOpID)); err != nil {
		t.Fatal(err)
	}
	if m.insertCalls.Load() != 1 {
		t.Fatalf("insert calls = %d, want 1 (terminated leftover must not be reused)", m.insertCalls.Load())
	}
}

func TestGCPCreate_PreflightFailureIsEffectNone(t *testing.T) {
	// A failure strictly before the insert is sent (here: canceled context
	// in image resolution) must be EffectNone — no uncertainty resolution
	// for a request that never left.
	_, g := newGCEMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.Apply(ctx, planCreate(t, g, "op_01FRESH"))
	if err == nil {
		t.Fatal("expected an error from a canceled context")
	}
	if provider.Effect(err) != provider.EffectNone {
		t.Fatalf("pre-flight side effect = %v (%v), want none", provider.Effect(err), err)
	}
}

// --- image resolution ---

func TestGCPImageResolution(t *testing.T) {
	_, g := newGCEMock(t)
	ctx := context.Background()

	for spec, want := range map[string]string{
		"id:custom-img":                     "projects/test-proj/global/images/custom-img",
		"id:projects/other/global/images/x": "projects/other/global/images/x",
		"family:debian-12":                  "https://compute.googleapis.com/compute/v1/projects/test-proj/global/images/debian-12-local",
		"family:debian-cloud/debian-12":     "https://compute.googleapis.com/compute/v1/projects/debian-cloud/global/images/debian-12-v20260801",
		"name:base-img":                     "https://compute.googleapis.com/compute/v1/projects/test-proj/global/images/base-img",
		"snapshot:Build=CI-Runner":          "https://compute.googleapis.com/compute/v1/projects/test-proj/global/images/img-new",
	} {
		got, err := g.image(ctx, spec)
		if err != nil {
			t.Fatalf("image(%q): %v", spec, err)
		}
		if got != want {
			t.Fatalf("image(%q) = %q, want %q", spec, got, want)
		}
	}

	for _, spec := range []string{
		"plainstring", "flavor:x", "snapshot:no-equals", "name:missing-img", "snapshot:build=nomatch", ":", "id:",
	} {
		_, err := g.image(ctx, spec)
		if !provider.IsClass(err, provider.ErrInvalid) {
			t.Fatalf("image(%q): err = %v, want ErrInvalid", spec, err)
		}
		if provider.Effect(err) != provider.EffectNone {
			t.Fatalf("image(%q): side effect = %v, want none", spec, provider.Effect(err))
		}
	}
}

// --- get / delete ---

func TestGCPGet404IsNotFound(t *testing.T) {
	m, g := newGCEMock(t)
	m.instanceGone.Store(true)
	_, err := g.Get(context.Background(), provider.ExternalRef{ID: "ci-1"})
	if !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if provider.Effect(err) != provider.EffectNone {
		t.Fatalf("side effect = %v, want none", provider.Effect(err))
	}
}

func TestGCPGetRateLimited(t *testing.T) {
	m, g := newGCEMock(t)
	m.get429.Store(true)
	_, err := g.Get(context.Background(), provider.ExternalRef{ID: "ci-1"})
	if !provider.IsClass(err, provider.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if ra, ok := provider.RetryAfterOf(err); !ok || ra != 7*time.Second {
		t.Fatalf("retry-after = %v (%v), want 7s from the header", ra, ok)
	}
}

func TestGCPDeleteOfDeletedIsSuccess(t *testing.T) {
	m, g := newGCEMock(t)
	m.instanceGone.Store(true)
	ref, err := g.Apply(context.Background(), provider.Action{
		ActionID: "op_01DEL", Kind: "delete",
		Ref: &provider.ExternalRef{ID: "ci-1", Extra: map[string]string{"zone": testZone}}, Destructive: true,
	})
	if err != nil {
		t.Fatalf("delete of deleted: %v", err)
	}
	if ref.Ref == nil || ref.Ref.ID != "ci-1" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestGCPDeleteEmitsOperation(t *testing.T) {
	_, g := newGCEMock(t)
	ref, err := g.Apply(context.Background(), provider.Action{
		ActionID: "op_01DEL", Kind: "delete",
		Ref: &provider.ExternalRef{ID: "ci-1", Extra: map[string]string{"zone": testZone}}, Destructive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var data opData
	if err := json.Unmarshal(ref.Data, &data); err != nil || data.Op != "op-del-1" || !data.Delete {
		t.Fatalf("delete op data = %s (err %v)", ref.Data, err)
	}
}

// --- discover ---

func TestGCPDiscover_OwnedScopeDecodesLabels(t *testing.T) {
	m, g := newGCEMock(t)
	out, err := g.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeOwned})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("discovered %d instances", len(out))
	}
	obs := out[0]
	if !obs.Owned || obs.FleetplaneID != testResID || obs.CreateOpID != takenOpID {
		t.Fatalf("labels not decoded/restored: %+v", obs)
	}
	if obs.Labels[provider.LabelOwner] != testOwner || obs.Labels[provider.LabelManaged] != "true" {
		t.Fatalf("labels = %v", obs.Labels)
	}
	if obs.Phase != provider.PhaseRunning || obs.ProviderState != "RUNNING" {
		t.Fatalf("phase = %v (%s)", obs.Phase, obs.ProviderState)
	}
	if obs.Capacity[compute.DimCPU] != 2 || obs.Capacity[compute.DimMemoryMiB] != 4096 {
		t.Fatalf("capacity = %v", obs.Capacity)
	}
	wantAddrs := []provider.Address{
		{Network: "private", Addr: "10.0.0.2"}, // the SDK network name, not "private-v4"
		{Network: "public-v4", Addr: "203.0.113.5"},
	}
	if !reflect.DeepEqual(obs.Addresses, wantAddrs) {
		t.Fatalf("addresses = %v", obs.Addresses)
	}
	if obs.Ref.Extra["zone"] != testZone {
		t.Fatalf("zone not decoded from the instance URL: %+v", obs.Ref)
	}
	if len(obs.Extensions) == 0 || !strings.Contains(string(obs.Extensions), `"ci-1"`) {
		t.Fatal("extensions missing the native object (invariant 6)")
	}
	filter, _ := m.lastFilter.Load().(string)
	if filter != `labels.fp-managed = "true" AND labels.fp-owner = "own_01owner"` {
		t.Fatalf("owned-scope filter = %q", filter)
	}
	// Discover must sweep the whole project (creates honor spec.location),
	// never just the default zone.
	if got, _ := m.lastListPath.Load().(string); !strings.HasSuffix(got, "/aggregated/instances") {
		t.Fatalf("discover listed %q, want the project-wide aggregated list", got)
	}
}

func TestGCPDiscover_SelectorMismatchFiltersOut(t *testing.T) {
	_, g := newGCEMock(t)
	out, err := g.Discover(context.Background(), provider.DiscoverRequest{
		Scope: provider.ScopeOwned, Selector: map[string]string{provider.LabelID: "res_01OTHER"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("client-side selector re-check failed: %v", out)
	}
}

// --- observe operation ---

func TestGCPObserveOperation_CreateSucceeds(t *testing.T) {
	_, g := newGCEMock(t)
	data, _ := json.Marshal(opData{V: 1, Op: "op-ins-1", Zone: testZone, Instance: "ci-1"})
	status, err := g.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: takenOpID,
		Ref:      &provider.ExternalRef{ID: "ci-1", Extra: map[string]string{"zone": testZone}},
		Data:     data,
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

func TestGCPObserveOperation_StillRunning(t *testing.T) {
	m, g := newGCEMock(t)
	m.opStatus.Store("RUNNING")
	data, _ := json.Marshal(opData{V: 1, Op: "op-ins-1", Zone: testZone, Instance: "ci-1"})
	status, err := g.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: takenOpID, Ref: &provider.ExternalRef{ID: "ci-1"}, Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpRunning || status.RetryAfter <= 0 {
		t.Fatalf("status = %+v", status)
	}
}

func TestGCPObserveOperation_CreateVanishedIsNotFound(t *testing.T) {
	m, g := newGCEMock(t)
	m.instanceGone.Store(true) // op DONE, instance gone: engine routes create+not_found to verifying
	data, _ := json.Marshal(opData{V: 1, Op: "op-ins-1", Zone: testZone, Instance: "ci-1"})
	_, err := g.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: takenOpID, Ref: &provider.ExternalRef{ID: "ci-1"}, Data: data,
	})
	if !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGCPObserveOperation_OperationErrorFails(t *testing.T) {
	m, g := newGCEMock(t)
	m.opError.Store(true)
	data, _ := json.Marshal(opData{V: 1, Op: "op-ins-1", Zone: testZone, Instance: "ci-1"})
	status, err := g.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: takenOpID, Ref: &provider.ExternalRef{ID: "ci-1"}, Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpFailed || status.Failure == nil {
		t.Fatalf("status = %+v", status)
	}
	if status.Failure.Class != provider.ErrQuota || status.Failure.Code != "QUOTA_EXCEEDED" {
		t.Fatalf("failure = %+v", status.Failure)
	}
}

func TestGCPObserveOperation_DataLostDegradesToInstance(t *testing.T) {
	_, g := newGCEMock(t)
	status, err := g.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: takenOpID, Ref: &provider.ExternalRef{ID: "ci-1", Extra: map[string]string{"zone": testZone}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpSucceeded || status.Resource == nil {
		t.Fatalf("status = %+v", status)
	}
}

// --- billing / descriptor ---

func TestGCPBillingPolicyContract(t *testing.T) {
	_, g := newGCEMock(t)
	if got := g.Billing(compute.Kind); got.MinimumDuration != time.Minute || got.BillingIncrement != 0 {
		t.Fatalf("compute.machine billing = %+v", got)
	}
	for _, kind := range []provider.ResourceKind{"storage.volume", "network.loadbalancer", "unknown.kind"} {
		if got := g.Billing(kind); !got.FineGrained() {
			t.Fatalf("Billing(%s) = %+v, want the zero policy", kind, got)
		}
	}
}

func TestGCPDescriptorAndHealth(t *testing.T) {
	_, g := newGCEMock(t)
	d := g.Descriptor()
	if d.Driver != "gcp" || d.Instance != "gcp-test" || !d.SupportsLabelDiscovery {
		t.Fatalf("descriptor = %+v", d)
	}
	if len(d.Kinds) != 1 || d.Kinds[0] != compute.Kind {
		t.Fatalf("kinds = %v", d.Kinds)
	}
	if err := g.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if _, ok := g.ResourceDriver(compute.Kind); !ok {
		t.Fatal("no driver for compute.machine")
	}
	if _, ok := g.ResourceDriver("storage.volume"); ok {
		t.Fatal("unexpected driver for storage.volume")
	}
}

func TestGCPPhaseMapping(t *testing.T) {
	// The full compute/v1 Instance.status enum — every state maps somewhere
	// deliberate.
	for status, want := range map[string]provider.ObservedPhase{
		"PENDING":        provider.PhasePending,
		"PROVISIONING":   provider.PhasePending,
		"STAGING":        provider.PhasePending,
		"RUNNING":        provider.PhaseRunning,
		"PENDING_STOP":   provider.PhaseStopped,
		"STOPPING":       provider.PhaseStopped,
		"SUSPENDING":     provider.PhaseStopped,
		"SUSPENDED":      provider.PhaseStopped,
		"STOPPED":        provider.PhaseStopped,
		"TERMINATED":     provider.PhaseStopped,
		"DEPROVISIONING": provider.PhaseStopped,
		"REPAIRING":      provider.PhaseUnknown,
	} {
		if got := mapStatus(status); got != want {
			t.Fatalf("mapStatus(%s) = %v, want %v", status, got, want)
		}
	}
}
