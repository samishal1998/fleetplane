package tests

// Phase-6 exit: external automation drives everything over HTTP with
// scoped tokens (07 §2–3). The authz matrix pins the route→permission map.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/samimishal/fleetplane/internal/api"
	"github.com/samimishal/fleetplane/internal/boot"
	"github.com/samimishal/fleetplane/internal/config"
	"github.com/samimishal/fleetplane/pkg/apiclient"
)

// startAuthServer boots a server with three tokens: admin, a read-only one,
// and an acquire-capable one.
func startAuthServer(t *testing.T) (base, adminTok, readTok, acquireTok string) {
	t.Helper()
	mk := func(name string, perms ...string) (string, api.TokenRecord) {
		set, err := api.NewPermSet(perms)
		if err != nil {
			t.Fatal(err)
		}
		plaintext, rec, err := api.GenerateToken(name, set)
		if err != nil {
			t.Fatal(err)
		}
		return plaintext, rec
	}
	adminTok, adminRec := mk("root", "admin")
	readTok, readRec := mk("reader", "resource.read", "pool.read", "operation.read", "provider.read")
	acquireTok, acqRec := mk("ci", "resource.read", "resource.acquire", "operation.read")

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
server: { addr: "127.0.0.1:0", opsAddr: "127.0.0.1:0", shutdownGrace: 2s }
storage: { path: %s }
providers:
  fake-local: { driver: fake }
classes:
  ci-large:
    kind: compute.machine
    provider: fake-local
    spec: { serverType: cpx31, image: "snapshot:ci=1" }
    reclaim: { idleAfter: 5m }
auth:
  tokens:
    - { id: %s, name: root, sha256: "%x", permissions: [admin] }
    - { id: %s, name: reader, sha256: "%x", permissions: [resource.read, pool.read, operation.read, provider.read] }
    - { id: %s, name: ci, sha256: "%x", permissions: [resource.read, resource.acquire, operation.read] }
`, filepath.Join(t.TempDir(), "fp.db"),
		adminRec.ID, adminRec.SHA256, readRec.ID, readRec.SHA256, acqRec.ID, acqRec.SHA256)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := boot.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := a.Listen(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
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
				return base, adminTok, readTok, acquireTok
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server never ready")
	return
}

func doReq(t *testing.T, method, url, token string, body []byte) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestAuthz_RouteMatrix(t *testing.T) {
	base, adminTok, readTok, acquireTok := startAuthServer(t)

	// No token: 401 on everything under /v1.
	if code := doReq(t, "GET", base+"/v1/resources", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", code)
	}
	// Garbage token: 401.
	if code := doReq(t, "GET", base+"/v1/resources", "flp_bogus.bogus", nil); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d, want 401", code)
	}
	// Reader can read everything readable...
	for _, path := range []string{"/v1/resources", "/v1/pools", "/v1/operations", "/v1/events", "/v1/providers"} {
		if code := doReq(t, "GET", base+path, readTok, nil); code != http.StatusOK {
			t.Fatalf("reader GET %s: %d, want 200", path, code)
		}
	}
	// ...but cannot mutate (403, and specifically NOT 401).
	if code := doReq(t, "POST", base+"/v1/acquisitions", readTok, []byte(`{"class":"ci-large"}`)); code != http.StatusForbidden {
		t.Fatalf("reader acquire: %d, want 403", code)
	}
	if code := doReq(t, "POST", base+"/v1/pools", readTok, []byte(`{"metadata":{"name":"p"},"spec":{"class":"ci-large","replicas":1}}`)); code != http.StatusForbidden {
		t.Fatalf("reader pool write: %d, want 403", code)
	}
	// The acquire token can acquire but not create pools or delete resources.
	if code := doReq(t, "POST", base+"/v1/acquisitions", acquireTok, []byte(`{"class":"ci-large"}`)); code != http.StatusCreated {
		t.Fatalf("ci acquire: %d, want 201", code)
	}
	if code := doReq(t, "POST", base+"/v1/pools", acquireTok, []byte(`{}`)); code != http.StatusForbidden {
		t.Fatalf("ci pool write: %d, want 403", code)
	}
	// Admin implies everything.
	if code := doReq(t, "POST", base+"/v1/pools", adminTok, []byte(`{"metadata":{"name":"p1"},"spec":{"class":"ci-large","replicas":0}}`)); code != http.StatusOK {
		t.Fatalf("admin pool write: %d, want 200", code)
	}
	// Health needs no token (network-gated, 07 §8).
	if code := doReq(t, "GET", base+"/health/ready", "", nil); code != http.StatusOK {
		t.Fatal("health requires auth; it must not")
	}
}

// The full Phase-6 exit path, HTTP only: acquire → watch → reuse → release
// → drain via colon verb → providers healthy.
func TestHTTP_AcquireLifecycle(t *testing.T) {
	base, adminTok, _, _ := startAuthServer(t)
	c := apiclient.New(base, adminTok)
	ctx := context.Background()

	acq, err := c.Acquire(ctx, apiclient.AcquireRequest{
		Class: "ci-large", Constraints: []byte(`{"cpu":{"min":1}}`),
		Lease: &apiclient.LeaseRequest{TTL: "30m"},
	}, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	// Poll to bound (the engine provisions via the fake).
	deadline := time.Now().Add(10 * time.Second)
	var got *apiclient.Acquisition
	for time.Now().Before(deadline) {
		got, err = c.GetAcquisition(ctx, acq.ID)
		if err == nil && got.State == "bound" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if got == nil || got.State != "bound" {
		t.Fatalf("acquisition never bound: %+v", got)
	}

	// Second acquire reuses (one machine total).
	acq2, err := c.Acquire(ctx, apiclient.AcquireRequest{
		Class: "ci-large", Constraints: []byte(`{"cpu":{"min":1}}`),
	}, "job-2")
	if err != nil {
		t.Fatal(err)
	}
	got2, err := c.GetAcquisition(ctx, acq2.ID)
	if err != nil || got2.State != "bound" {
		t.Fatalf("second acquire: %+v %v", got2, err)
	}
	list, err := c.ListResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("reuse failed: %d machines", len(list.Items))
	}
	resID := list.Items[0].Metadata.ID

	// Release both; drain via the colon verb (ADR-005).
	if err := c.Release(ctx, acq.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx, acq2.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DrainResource(ctx, resID); err != nil {
		t.Fatalf("drain colon-verb route: %v", err)
	}
	res, err := c.GetResource(ctx, resID)
	if err != nil || res.Status.Phase != "draining" {
		t.Fatalf("after drain: %+v %v", res, err)
	}

	providers, err := c.ListProviders(ctx)
	if err != nil || len(providers) != 1 || providers[0].State != "healthy" {
		t.Fatalf("providers: %+v %v", providers, err)
	}
}
