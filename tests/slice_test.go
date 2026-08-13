// Package tests holds cross-package integration tests (the vertical slice
// and, later, the failure-injection catalog). It lives outside internal/ so
// importing providers/fake here never weakens the kernel-purity rules.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/samimishal/fleetplane/internal/app"
	"github.com/samimishal/fleetplane/internal/boot"
	"github.com/samimishal/fleetplane/internal/config"
	"github.com/samimishal/fleetplane/internal/operations"
	"github.com/samimishal/fleetplane/internal/phase"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/internal/storage/sqlite"
	"github.com/samimishal/fleetplane/pkg/apiclient"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
	"github.com/samimishal/fleetplane/pkg/sdk/secretref"

	_ "github.com/samimishal/fleetplane/providers/fake"
)

const machineSpec = `{"serverType":"cpx31","image":"snapshot:ci=1"}`

func createBody(name string) []byte {
	return []byte(fmt.Sprintf(
		`{"spec":{"kind":"compute.machine","provider":"fake-local","machine":%s},"metadata":{"name":%q}}`,
		machineSpec, name))
}

// --- boot-level slice: API → engine → fake → ready → delete ---

func startServer(t *testing.T) (base string, cancel context.CancelFunc) {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
server: { addr: "127.0.0.1:0", opsAddr: "127.0.0.1:0", shutdownGrace: 2s }
storage: { path: %s }
providers:
  fake-local: { driver: fake }
`, filepath.Join(t.TempDir(), "fp.db"))))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelCtx := context.WithCancel(context.Background())
	a, err := boot.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		cancelCtx()
		t.Fatal(err)
	}
	if err := a.Listen(); err != nil {
		cancelCtx()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	t.Cleanup(func() {
		cancelCtx()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down")
		}
	})
	base = "http://" + a.MainAddr().String()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health/ready")
		if err == nil {
			code := resp.StatusCode
			_ = resp.Body.Close()
			if code == http.StatusOK {
				return base, cancelCtx
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server never ready")
	return "", nil
}

func waitPhase(t *testing.T, c *apiclient.Client, id, want string) *apiclient.Resource {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last *apiclient.Resource
	for time.Now().Before(deadline) {
		res, err := c.GetResource(context.Background(), id)
		if err == nil {
			last = res
			if res.Status.Phase == want {
				return res
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("resource %s never reached phase %q (last: %+v)", id, want, last)
	return nil
}

func TestSlice_CreateToReadyThenDelete(t *testing.T) {
	base, _ := startServer(t)
	c := apiclient.New(base, "")

	var req apiclient.CreateResourceRequest
	if err := json.Unmarshal(createBody("slice-1"), &req); err != nil {
		t.Fatal(err)
	}
	res, err := c.CreateResource(context.Background(), req, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status.Phase != string(phase.Provisioning) {
		t.Fatalf("fresh resource phase = %s, want provisioning", res.Status.Phase)
	}

	ready := waitPhase(t, c, res.Metadata.ID, string(phase.Ready))
	if ready.Status.ExternalID == "" {
		t.Fatal("ready resource has no external id")
	}
	if len(ready.Status.Extensions) == 0 {
		t.Fatal("provider extensions missing from envelope (invariant 6)")
	}

	if err := c.DeleteResource(context.Background(), res.Metadata.ID, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := c.GetResource(context.Background(), res.Metadata.ID)
		if err == nil && got.Metadata.DeletedAt != nil {
			return // tombstoned (ADR-017)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("resource never tombstoned after delete")
}

func TestSlice_IdempotentCreateReplaysBytes(t *testing.T) {
	base, _ := startServer(t)
	body := createBody("idem-1")

	post := func() (int, []byte, string) {
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/resources", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "job-42")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header.Get("Idempotency-Replayed")
	}

	code1, body1, replayed1 := post()
	if code1 != http.StatusCreated || replayed1 != "" {
		t.Fatalf("first POST: %d replayed=%q", code1, replayed1)
	}
	code2, body2, replayed2 := post()
	if code2 != http.StatusCreated || replayed2 != "true" {
		t.Fatalf("second POST: %d replayed=%q", code2, replayed2)
	}
	if !bytes.Equal(body1, body2) {
		t.Fatalf("replay is not byte-identical (invariant 2):\n%s\n%s", body1, body2)
	}

	// Exactly one resource exists.
	c := apiclient.New(base, "")
	list, err := c.ListResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("idempotent replay created %d resources, want 1", len(list.Items))
	}

	// Same key, different payload → 409 idempotency_mismatch.
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/resources", bytes.NewReader(createBody("other-name")))
	req.Header.Set("Idempotency-Key", "job-42")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("mismatched payload reuse: %d, want 409", resp.StatusCode)
	}
}

// --- crash recovery (in-process tier; subprocess tier lands at I6) ---

type pieces struct {
	st        storage.Store
	providers *app.Providers
	svc       *app.Service
	engine    *operations.Engine
}

// build assembles the stack over an existing DB file; calling it again on
// the same path models a process restart (provider-side state lives in the
// shared fake instance registry only within one build, so crash tests
// reuse the SAME providers across "restarts" — the cloud outlives us).
func build(t *testing.T, dbPath string, providers *app.Providers, hooks operations.Hooks) pieces {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.OpenStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ownerID, err := app.EnsureOwnerID(ctx, st, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if providers == nil {
		providers, err = app.BuildProviders(ctx, st,
			[]app.ProviderSpec{{Name: "fake-local", Driver: "fake"}},
			ownerID, secretref.NewDefault(), log, time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}
	engine := operations.New(st, providers, tickClock{}, log, operations.Config{}, hooks)
	svc := app.NewService(st, providers, engine, tickClock{}, log, ownerID)
	return pieces{st: st, providers: providers, svc: svc, engine: engine}
}

// tickClock skews Now() forward aggressively so persisted backoff deadlines
// are always due in tests without sleeping.
type tickClock struct{}

func (tickClock) Now() time.Time { return time.Now().Add(10 * time.Minute) }
func (tickClock) After(d time.Duration) <-chan time.Time {
	return time.After(10 * time.Millisecond)
}

func create(t *testing.T, p pieces, name string) storage.ResourceID {
	t.Helper()
	out, err := p.svc.CreateResource(context.Background(), app.CreateResourceCmd{
		Kind: "compute.machine", Provider: "fake-local", Name: name,
		Spec: json.RawMessage(machineSpec), Actor: "test",
		BuildResponse: func(r *storage.Resource) (int, json.RawMessage) {
			b, _ := json.Marshal(map[string]string{"id": string(r.ID)})
			return 201, b
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ ID string }
	if err := json.Unmarshal(out.Body, &body); err != nil {
		t.Fatal(err)
	}
	return storage.ResourceID(body.ID)
}

func driveToTerminal(t *testing.T, p pieces, resID storage.ResourceID) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if _, err := p.engine.Step(ctx); err != nil {
			t.Fatal(err)
		}
		ops, err := p.st.Operations().NonTerminal(ctx)
		if err != nil {
			t.Fatal(err)
		}
		open := 0
		for _, op := range ops {
			if op.ResourceID != nil && *op.ResourceID == resID {
				open++
			}
		}
		if open == 0 {
			return
		}
	}
	t.Fatal("operation never reached a terminal state")
}

func countFakeObjects(t *testing.T, p pieces) int {
	t.Helper()
	inst, ok := p.providers.Instance("fake-local")
	if !ok {
		t.Fatal("fake instance missing")
	}
	driver, _ := inst.ResourceDriver(provider.ResourceKind("compute.machine"))
	objs, err := driver.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	return len(objs)
}

// FI-2 shape: crash after journal write, before the provider call. The
// journaled state proves the provider was never called → safe re-dispatch.
func TestCrash_AfterJournal_RedispatchExactlyOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fp.db")
	p1 := build(t, dbPath, nil, operations.Hooks{})
	resID := create(t, p1, "crashy-1") // TxA committed; engine never ran
	_ = p1.st.Close()                  // hard stop before dispatch

	p2 := build(t, dbPath, p1.providers, operations.Hooks{})
	if err := p2.engine.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	driveToTerminal(t, p2, resID)

	if n := countFakeObjects(t, p2); n != 1 {
		t.Fatalf("recovery created %d provider objects, want exactly 1 (invariant 7)", n)
	}
	res, err := p2.st.Resources().Get(context.Background(), resID)
	if err != nil || res.Phase != phase.Ready {
		t.Fatalf("resource after recovery: %+v %v", res, err)
	}
}

// FI-3 shape: crash after the provider accepted the create but before the
// ref was committed (in_flight). Recovery must adopt by the op label —
// never blind-retry into a duplicate.
func TestCrash_AfterProviderAccept_AdoptsByOpLabel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fp.db")

	crash := struct{ armed bool }{armed: true}
	p1 := build(t, dbPath, nil, operations.Hooks{
		AfterApply: func(storage.OperationID) {
			if crash.armed {
				panic("simulated crash between Apply and TxC")
			}
		},
	})
	resID := create(t, p1, "crashy-2")

	func() {
		defer func() { _ = recover() }() // the "process" dies mid-dispatch
		_, _ = p1.engine.Step(context.Background())
	}()
	if n := countFakeObjects(t, p1); n != 1 {
		t.Fatalf("precondition: provider object should exist after Apply, got %d", n)
	}
	op, err := firstOpFor(p1.st, resID)
	if err != nil || op.State != storage.OpInFlight {
		t.Fatalf("precondition: op state %v, want in_flight (%v)", op, err)
	}
	_ = p1.st.Close()

	crash.armed = false
	p2 := build(t, dbPath, p1.providers, operations.Hooks{})
	if err := p2.engine.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	driveToTerminal(t, p2, resID)

	if n := countFakeObjects(t, p2); n != 1 {
		t.Fatalf("recovery produced %d provider objects, want 1 — blind duplicate (invariant 7)", n)
	}
	res, _ := p2.st.Resources().Get(context.Background(), resID)
	if res.Phase != phase.Ready || res.ExternalID == nil {
		t.Fatalf("adopted resource: %+v", res)
	}
}

func firstOpFor(st storage.Store, resID storage.ResourceID) (*storage.Operation, error) {
	ops, err := st.Operations().NonTerminal(context.Background())
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		if op.ResourceID != nil && *op.ResourceID == resID {
			return op, nil
		}
	}
	return nil, fmt.Errorf("no open operation for %s", resID)
}
