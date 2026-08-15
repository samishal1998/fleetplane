package tests

// Subprocess crash tier: the real binary, killed with SIGKILL mid-create,
// restarted over the same database. Proves WAL recovery + journal Resume
// through an actual process boundary (the in-process tier in fi_test.go
// covers duplicate-detection precisely, since the fake cloud there
// survives the crash).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/pkg/apiclient"
)

func TestSubprocess_Kill9DuringCreate_RecoversToReady(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess tier skipped in -short")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fleetplane")
	buildCmd := exec.Command("go", "build", "-o", bin, "github.com/samishal1998/fleetplane/cmd/fleetplane")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	const addr, opsAddr = "127.0.0.1:18191", "127.0.0.1:19191"
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`
server: { addr: "%s", opsAddr: "%s", shutdownGrace: 1s }
storage: { path: %s }
engine: { verifyWindow: 2s, pollInterval: 200ms }
providers:
  fake-local:
    driver: fake
    settings: { createSteps: 3 }
`, addr, opsAddr, filepath.Join(dir, "fp.db"))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr

	start := func() *exec.Cmd {
		cmd := exec.Command(bin, "serve", "--config", cfgPath, "--log-level", "error")
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Get(base + "/health/ready")
			if err == nil {
				code := resp.StatusCode
				_ = resp.Body.Close()
				if code == http.StatusOK {
					return cmd
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = cmd.Process.Kill()
		t.Fatal("server never became ready")
		return nil
	}

	srv := start()

	// Journal a create; the fake needs 3 observation steps, so the op is
	// mid-poll when we kill.
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/resources", bytes.NewReader(createBody("kill9")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created apiclient.Resource
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	time.Sleep(600 * time.Millisecond)         // let dispatch land (op in/after TxB)
	if err := srv.Process.Kill(); err != nil { // SIGKILL: no shutdown hooks
		t.Fatal(err)
	}
	_, _ = srv.Process.Wait()

	srv2 := start() // recovery: WAL replay + Engine.Resume before readiness
	defer func() {
		_ = srv2.Process.Kill()
		_, _ = srv2.Process.Wait()
	}()

	c := apiclient.New(base, "")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := c.GetResource(context.Background(), created.Metadata.ID)
		if err == nil && res.Status.Phase == "ready" && res.Status.ExternalID != "" {
			list, err := c.ListResources(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 1 {
				t.Fatalf("recovery left %d resources, want 1", len(list.Items))
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("resource never converged to ready after kill -9 recovery")
}
