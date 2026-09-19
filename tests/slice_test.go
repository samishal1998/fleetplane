// Package tests holds cross-package integration tests (the vertical slice
// and the failure-injection catalog). It lives outside internal/ so
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

	"github.com/samishal1998/fleetplane/internal/boot"
	"github.com/samishal1998/fleetplane/internal/config"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/pkg/apiclient"

	_ "github.com/samishal1998/fleetplane/providers/fake"
)

const machineSpec = `{"serverType":"cpx31","image":"snapshot:ci=1"}`

func createBody(name string) []byte {
	return []byte(fmt.Sprintf(
		`{"spec":{"kind":"compute.machine","provider":"fake-local","machine":%s},"metadata":{"name":%q}}`,
		machineSpec, name))
}

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

	c := apiclient.New(base, "")
	list, err := c.ListResources(context.Background(), apiclient.ResourceFilter{})
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
