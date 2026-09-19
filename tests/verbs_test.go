package tests

// The operator verbs and list filters that had no HTTP surface until the
// capability audit: protect/unprotect, undrain, pool pause/delete, the
// resource list filters and the acquisition list. Each test proves the call
// GATES something — a refused delete, a reused machine, a narrowed list —
// because a verb that only returns 200 is indistinguishable from a no-op.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/pkg/apiclient"
)

func makeResource(t *testing.T, c *apiclient.Client, name string) string {
	t.Helper()
	res, err := c.CreateResource(context.Background(), apiclient.CreateResourceRequest{
		Metadata: apiclient.Metadata{Name: name},
		Spec:     apiclient.ResourceSpec{Class: "ci-large"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	return res.Metadata.ID
}

// delete_protected gated deletion, parking, exclusive scheduling and idle
// reclaim long before any API could set it. The proof is the refused delete,
// not the 200 from :protect.
func TestHTTP_ProtectGatesDelete(t *testing.T) {
	base, adminTok, _, _ := startAuthServer(t)
	c := apiclient.New(base, adminTok)
	ctx := context.Background()

	id := makeResource(t, c, "protected")
	waitPhase(t, c, id, "ready") // ready→deleting is legal, so a 409 can only come from protection

	if err := c.ProtectResource(ctx, id); err != nil {
		t.Fatal(err)
	}
	if res, err := c.GetResource(ctx, id); err != nil || !res.Metadata.Protected {
		t.Fatalf("metadata.protected after :protect = %+v (%v)", res, err)
	}
	if code := doReq(t, http.MethodDelete, base+"/v1/resources/"+id, adminTok, nil); code != http.StatusConflict {
		t.Fatalf("delete of a protected resource: %d, want 409", code)
	}
	// Asking for the state it is already in is success, not a conflict.
	if code := doReq(t, http.MethodPost, base+"/v1/resources/"+id+":protect", adminTok, nil); code != http.StatusOK {
		t.Fatalf("re-protect: %d, want 200", code)
	}

	if err := c.UnprotectResource(ctx, id); err != nil {
		t.Fatal(err)
	}
	if code := doReq(t, http.MethodDelete, base+"/v1/resources/"+id, adminTok, nil); code != http.StatusAccepted {
		t.Fatalf("delete after :unprotect: %d, want 202", code)
	}
}

// Undrain has to put the machine back in service, not just relabel it: the
// proof is that the next acquisition binds to the SAME machine instead of
// provisioning a second one.
func TestHTTP_UndrainReturnsResourceToService(t *testing.T) {
	base, adminTok, _, _ := startAuthServer(t)
	c := apiclient.New(base, adminTok)
	ctx := context.Background()

	acq, err := c.Acquire(ctx, apiclient.AcquireRequest{
		Class: "ci-large", Lease: &apiclient.LeaseRequest{TTL: "30m"},
	}, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	bound := waitAcqState(t, c, acq.ID, "bound")
	resID := bound.ResourceID
	if resID == "" {
		t.Fatal("bound acquisition carries no resource id")
	}
	if err := c.Release(ctx, acq.ID); err != nil {
		t.Fatal(err)
	}

	if err := c.DrainResource(ctx, resID); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, c, resID, "draining")
	if err := c.UndrainResource(ctx, resID); err != nil {
		t.Fatalf("undrain: %v", err)
	}
	waitPhase(t, c, resID, "ready")

	acq2, err := c.Acquire(ctx, apiclient.AcquireRequest{
		Class: "ci-large", Lease: &apiclient.LeaseRequest{TTL: "30m"},
	}, "job-2")
	if err != nil {
		t.Fatal(err)
	}
	bound2 := waitAcqState(t, c, acq2.ID, "bound")
	if bound2.ResourceID != resID {
		t.Fatalf("acquisition bound to %s, want the undrained %s", bound2.ResourceID, resID)
	}
	list, err := c.ListResources(ctx, apiclient.ResourceFilter{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("undrain left the machine unusable: %d machines exist (%v)", len(list.Items), err)
	}
}

func waitAcqState(t *testing.T, c *apiclient.Client, id, want string) *apiclient.Acquisition {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last *apiclient.Acquisition
	for time.Now().Before(deadline) {
		acq, err := c.GetAcquisition(context.Background(), id)
		if err == nil {
			last = acq
			if acq.State == want {
				return acq
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("acquisition %s never reached %s: %+v", id, want, last)
	return nil
}

// ?pool= and ?phase= must narrow server-side: the dashboard fell back to
// listing by class because the handler exposed neither, and a filter that
// quietly returns the whole fleet is worse than no filter.
func TestHTTP_ResourceListFilters(t *testing.T) {
	base, adminTok, _, _ := startAuthServer(t)
	c := apiclient.New(base, adminTok)
	ctx := context.Background()

	pool, err := c.ApplyPool(ctx, apiclient.PoolManifest{
		Metadata: apiclient.Metadata{Name: "filtered"},
		Spec:     []byte(`{"class":"ci-large","replicas":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	solo := makeResource(t, c, "poolless")

	// Wait on the unfiltered list so a broken filter cannot satisfy the poll.
	deadline := time.Now().Add(10 * time.Second)
	total := 0
	for time.Now().Before(deadline) && total != 3 {
		all, err := c.ListResources(ctx, apiclient.ResourceFilter{})
		if err == nil {
			total = len(all.Items)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if total != 3 {
		t.Fatalf("fleet reached %d resources, want the pool's 2 plus the poolless one", total)
	}

	members, err := c.ListResources(ctx, apiclient.ResourceFilter{Pool: pool.Metadata.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 2 {
		t.Fatalf("?pool= returned %d resources, want the pool's 2", len(members.Items))
	}
	for _, r := range members.Items {
		if r.Metadata.ID == solo {
			t.Fatal("?pool= returned a poolless resource")
		}
	}

	waitPhase(t, c, solo, "ready")
	if code := doReq(t, http.MethodPost, base+"/v1/resources/"+solo+":park", adminTok, nil); code != http.StatusAccepted {
		t.Fatalf("park: %d, want 202", code)
	}
	waitPhase(t, c, solo, "parked")
	parked, err := c.ListResources(ctx, apiclient.ResourceFilter{Phases: []string{"parked"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parked.Items) != 1 || parked.Items[0].Metadata.ID != solo {
		t.Fatalf("?phase=parked returned %d items, want only %s", len(parked.Items), solo)
	}

	// An unknown phase is a typo, not an empty result set.
	if code := doReq(t, http.MethodGet, base+"/v1/resources?phase=parkd", adminTok, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown phase: %d, want 400", code)
	}
}

// The acquisition list had no route at all. Two things make it useful: a
// bound acquisition must carry both its lease and its resource (Bind nulls
// pending_resource_id, so the resource id is back-filled from the lease), and
// the unfiltered list must show only what is live — acquisitions are never
// garbage collected.
func TestHTTP_AcquisitionList(t *testing.T) {
	base, adminTok, _, _ := startAuthServer(t)
	c := apiclient.New(base, adminTok)
	ctx := context.Background()

	acq, err := c.Acquire(ctx, apiclient.AcquireRequest{
		Class: "ci-large", Lease: &apiclient.LeaseRequest{TTL: "30m"},
	}, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	bound := waitAcqState(t, c, acq.ID, "bound")

	live, err := c.ListAcquisitions(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	got := findAcq(live.Items, acq.ID)
	if got == nil {
		t.Fatalf("bound acquisition missing from the live list: %+v", live.Items)
	}
	if got.ResourceID == "" || got.LeaseID == "" {
		t.Fatalf("bound acquisition listed without resource/lease: %+v", got)
	}
	if got.ResourceID != bound.ResourceID {
		t.Fatalf("listed resource id %s, want %s", got.ResourceID, bound.ResourceID)
	}

	byRes, err := c.ListAcquisitions(ctx, nil, got.ResourceID)
	if err != nil || findAcq(byRes.Items, acq.ID) == nil {
		t.Fatalf("?resource= dropped the acquisition holding it: %+v %v", byRes, err)
	}
	other, err := c.ListAcquisitions(ctx, nil, "res_nobody")
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("?resource= of an unrelated id returned %+v (%v)", other, err)
	}

	// Released acquisitions leave the default list but stay retrievable.
	if err := c.Release(ctx, acq.ID); err != nil {
		t.Fatal(err)
	}
	live, err = c.ListAcquisitions(ctx, nil, "")
	if err != nil || findAcq(live.Items, acq.ID) != nil {
		t.Fatalf("released acquisition still in the live list: %+v %v", live.Items, err)
	}
	released, err := c.ListAcquisitions(ctx, []string{"released"}, "")
	if err != nil || findAcq(released.Items, acq.ID) == nil {
		t.Fatalf("?state=released missed it: %+v %v", released, err)
	}
	if code := doReq(t, http.MethodGet, base+"/v1/acquisitions?state=bounded", adminTok, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown state: %d, want 400", code)
	}
}

func findAcq(items []apiclient.Acquisition, id string) *apiclient.Acquisition {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

// Pools could never be removed at any layer. Deleting one the reconciler is
// still creating into would strand its in-flight resource on a pool row that
// no longer exists, so replicas > 0 is a conflict even before any member
// exists. (The scaled-down-with-tombstoned-members path is proven in
// TestPool_DeleteGatesOnReplicasAndMembers.)
func TestHTTP_PoolDelete(t *testing.T) {
	base, adminTok, _, _ := startAuthServer(t)
	c := apiclient.New(base, adminTok)
	ctx := context.Background()

	busy, err := c.ApplyPool(ctx, apiclient.PoolManifest{
		Metadata: apiclient.Metadata{Name: "busy"},
		Spec:     []byte(`{"class":"ci-large","replicas":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := doReq(t, http.MethodDelete, base+"/v1/pools/"+busy.Metadata.ID, adminTok, nil); code != http.StatusConflict {
		t.Fatalf("delete of a pool wanting replicas: %d, want 409", code)
	}

	idle, err := c.ApplyPool(ctx, apiclient.PoolManifest{
		Metadata: apiclient.Metadata{Name: "idle"},
		Spec:     []byte(`{"class":"ci-large","replicas":0}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeletePool(ctx, idle.Metadata.ID); err != nil {
		t.Fatalf("delete of an empty pool: %v", err)
	}
	if code := doReq(t, http.MethodGet, base+"/v1/pools/"+idle.Metadata.ID, adminTok, nil); code != http.StatusNotFound {
		t.Fatalf("deleted pool still readable: %d, want 404", code)
	}
}

// Pause is refused a reconcile kick rather than silently swallowing it — the
// 202 the route used to return told the caller a cycle was coming that the
// paused reconciler would discard.
func TestHTTP_PausedPoolRefusesReconcile(t *testing.T) {
	base, adminTok, _, _ := startAuthServer(t)
	c := apiclient.New(base, adminTok)
	ctx := context.Background()

	pool, err := c.ApplyPool(ctx, apiclient.PoolManifest{
		Metadata: apiclient.Metadata{Name: "frozen"},
		Spec:     []byte(`{"class":"ci-large","replicas":0}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := pool.Metadata.ID
	if err := c.PausePool(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetPool(ctx, id); err != nil || !got.Paused {
		t.Fatalf("pool.paused after :pause = %+v (%v)", got, err)
	}
	if code := doReq(t, http.MethodPost, base+"/v1/pools/"+id+":reconcile", adminTok, nil); code != http.StatusConflict {
		t.Fatalf("reconcile of a paused pool: %d, want 409", code)
	}
	// Pause lives outside the manifest precisely so an apply cannot clear it:
	// `paused` has no "unset", so a spec without the field must not resume.
	if _, err := c.ApplyPool(ctx, apiclient.PoolManifest{
		Metadata: apiclient.Metadata{Name: "frozen"},
		Spec:     []byte(`{"class":"ci-large","replicas":0}`),
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetPool(ctx, id); err != nil || !got.Paused {
		t.Fatalf("apply resumed a paused pool: %+v (%v)", got, err)
	}
	if err := c.ResumePool(ctx, id); err != nil {
		t.Fatal(err)
	}
	if code := doReq(t, http.MethodPost, base+"/v1/pools/"+id+":reconcile", adminTok, nil); code != http.StatusAccepted {
		t.Fatalf("reconcile after :resume: %d, want 202", code)
	}
}
