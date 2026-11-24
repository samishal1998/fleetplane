package tests

// Control-plane E2E against REAL clouds through the REAL binary: each
// scenario boots `fleetplane serve` as a subprocess with one provider
// (docker, hetzner or gcp), drives it over HTTP, and checks the cloud itself
// — not the database — for the outcome. Crashes are SIGKILLs of that
// process. Docker needs no credentials, so every scenario is validated
// locally before it costs money; the cloud targets are gated exactly like
// their conformance tests (ADR-015).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/pkg/apiclient"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/providers/docker"
)

// cloudTarget is one provider under test.
type cloudTarget struct {
	name     string // provider instance name in the shipped config
	provider string // YAML flow map: driver, settings, billing
	base     string // compute.machine spec JSON fields minus image and labels
	image    string // default image
	ubuntu   string // a cloud-init image ("" = target has no cloud-init)
	parks    bool   // stop/start supported → the warm tier is exercised
	wait     time.Duration
	driver   provider.ResourceDriver // ground truth: ScopeAll + run label
	runID    string
	// snapshot images the (stopped) machine and returns the spec image
	// selector that boots it; it registers its own cleanup. nil = unsupported.
	snapshot func(t *testing.T, ref provider.ExternalRef, runID string) string
}

func cloudTargets(t *testing.T) map[string]func(*testing.T) *cloudTarget {
	return map[string]func(*testing.T) *cloudTarget{
		"docker": func(t *testing.T) *cloudTarget {
			d, err := docker.New(provider.InstanceConfig{Instance: "observer"})
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Health(context.Background()); err != nil {
				if os.Getenv("FLEETPLANE_E2E_DOCKER") == "1" {
					t.Fatalf("docker unreachable: %v", err)
				}
				t.Skipf("docker unreachable: %v", err)
			}
			drv, _ := d.ResourceDriver("compute.machine")
			return &cloudTarget{
				name: "docker-e2e", provider: `{driver: docker}`,
				base: `"serverType":"1x64"`, image: "name:alpine:3.20",
				parks: true, wait: time.Minute, driver: drv,
			}
		},
		"hetzner": func(t *testing.T) *cloudTarget {
			h := e2eProvider(t, "e2e-observer") // skips unless gated in
			drv, _ := h.ResourceDriver("compute.machine")
			loc := e2eHetznerLocation()
			return &cloudTarget{
				name: "hetzner-e2e",
				// Hetzner bills by the hour: billing-aware reclaim would
				// hold an idle machine until its paid hour ends.
				provider: fmt.Sprintf(`{driver: hetzner, settings: {token: "secret://env/HETZNER_TOKEN", location: %s}, billing: {compute.machine: {disabled: true}}}`, loc),
				base: fmt.Sprintf(`"serverType":%q,"location":%q`,
					envOr("HETZNER_E2E_SERVER_TYPE", "cpx22"), loc),
				image: "name:ubuntu-24.04", ubuntu: "name:ubuntu-24.04",
				snapshot: hetznerSnapshot,
				parks:    false, // Hetzner bills stopped servers; the driver has no stop
				wait:     6 * time.Minute, driver: drv,
			}
		},
		"gcp": func(t *testing.T) *cloudTarget {
			g := gcpE2EProvider(t, "e2e-observer") // skips unless gated in
			drv, _ := g.ResourceDriver("compute.machine")
			key, _ := filepath.Abs(os.Getenv("GCP_E2E_CREDENTIALS_FILE"))
			return &cloudTarget{
				name: "gcp-e2e",
				provider: fmt.Sprintf(`{driver: gcp, settings: {project: %q, zone: %q, credentialsJson: "secret://file%s"}}`,
					os.Getenv("GCP_E2E_PROJECT"), gcpE2EZone(), key),
				base: `"serverType":"e2-micro"`, image: "family:debian-cloud/debian-12",
				ubuntu:   "family:ubuntu-os-cloud/ubuntu-2404-lts-amd64",
				snapshot: gcpSnapshot,
				parks:    true, wait: 8 * time.Minute, driver: drv,
			}
		},
	}
}

// eachCloud runs scenario once per target; cloud targets run in parallel
// (separate projects, separate run labels).
func eachCloud(t *testing.T, scenario func(*testing.T, *cloudTarget, *fpServer)) {
	if testing.Short() {
		t.Skip("real-binary cloud scenarios skipped in -short")
	}
	for name, mk := range cloudTargets(t) {
		t.Run(name, func(t *testing.T) {
			if name != "docker" {
				t.Parallel()
			}
			tgt := mk(t)
			// Lowercase, [a-z0-9-] only: valid as a label value on every
			// cloud and as a GCP image name.
			tgt.runID = fmt.Sprintf("r%d-%s", time.Now().UnixNano()%1e9,
				strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(strings.TrimPrefix(t.Name(), "TestE2E_"))))
			if len(tgt.runID) > 60 {
				tgt.runID = tgt.runID[:60]
			}
			t.Cleanup(func() { tgt.sweep(t) })
			scenario(t, tgt, startFP(t, tgt))
		})
	}
}

// machineSpec is the class spec with the labels every test machine carries
// (fleetplane.io/test drives the sweeper; test-run scopes ground truth).
func (c *cloudTarget) machineSpec() string { return c.machineWith(c.image, "") }

func (c *cloudTarget) machineWith(image, userData string) string {
	ud := ""
	if userData != "" {
		ud = fmt.Sprintf(`,"userData":%q`, userData)
	}
	return fmt.Sprintf(`{%s,"image":%q%s,"labels":{"fleetplane.io/test":"1","fleetplane.io/test-run":%q}}`,
		c.base, image, ud, c.runID)
}

// createInline creates a classless resource with an explicit machine spec.
func (s *fpServer) createInline(t *testing.T, tgt *cloudTarget, name, machine string) string {
	t.Helper()
	res, err := s.c.CreateResource(context.Background(), apiclient.CreateResourceRequest{
		Metadata: apiclient.Metadata{Name: name},
		Spec: apiclient.ResourceSpec{Kind: "compute.machine", Provider: tgt.name,
			Machine: json.RawMessage(machine)},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	return res.Metadata.ID
}

// machineOf returns the cloud's view of one resource's machine.
func (c *cloudTarget) machineOf(t *testing.T, resID string) (provider.ObservedResource, bool) {
	for _, m := range c.live(t) {
		if m.FleetplaneID == resID {
			return m, true
		}
	}
	return provider.ObservedResource{}, false
}

func (c *cloudTarget) waitMachinePhase(t *testing.T, resID string, want provider.ObservedPhase, why string) provider.ObservedResource {
	t.Helper()
	var m provider.ObservedResource
	eventually(t, c.wait, fmt.Sprintf("%s machine %s (%s)", resID, want, why), func() (bool, string) {
		var ok bool
		m, ok = c.machineOf(t, resID)
		return ok && m.Phase == want, summarize(c.live(t))
	})
	return m
}

// live is the cloud's own view: every non-deleted machine of this run.
func (c *cloudTarget) live(t *testing.T) []provider.ObservedResource {
	t.Helper()
	list, err := c.driver.Discover(context.Background(), provider.DiscoverRequest{
		Scope: provider.ScopeAll, Selector: map[string]string{"fleetplane.io/test-run": c.runID},
	})
	if err != nil {
		t.Fatalf("cloud discover: %v", err)
	}
	out := list[:0]
	for _, m := range list {
		if m.Phase != provider.PhaseGone && m.Phase != provider.PhaseDeleting {
			out = append(out, m)
		}
	}
	return out
}

// sweep deletes whatever this run left behind, so a failed scenario never
// leaks a billed machine (the e2esweep backstop covers a killed test).
func (c *cloudTarget) sweep(t *testing.T) {
	list, err := c.driver.Discover(context.Background(), provider.DiscoverRequest{
		Scope: provider.ScopeAll, Selector: map[string]string{"fleetplane.io/test-run": c.runID},
	})
	if err != nil {
		t.Errorf("cleanup discover: %v", err)
		return
	}
	for _, m := range list {
		if m.Phase == provider.PhaseGone {
			continue
		}
		if _, err := c.driver.Apply(context.Background(), provider.Action{
			ActionID: "e2e-sweep", Kind: "delete", Ref: &m.Ref, Destructive: true,
		}); err != nil && !provider.IsClass(err, provider.ErrNotFound) {
			t.Errorf("cleanup delete %s: %v", m.Ref.ID, err)
		}
	}
}

// --- the binary under test ---

var (
	fpBinOnce sync.Once
	fpBin     string
	fpBinErr  error
)

type fpServer struct {
	t        *testing.T
	base     string
	cfgPath  string
	cmd      *exec.Cmd
	c        *apiclient.Client
	stopOnce sync.Once
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fpBin != "" {
		_ = os.RemoveAll(filepath.Dir(fpBin))
	}
	os.Exit(code)
}

func startFP(t *testing.T, tgt *cloudTarget) *fpServer {
	t.Helper()
	fpBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fp-e2e-bin-")
		if err != nil {
			fpBinErr = err
			return
		}
		fpBin = filepath.Join(dir, "fleetplane")
		out, err := exec.Command("go", "build", "-o", fpBin, "github.com/samishal1998/fleetplane/cmd/fleetplane").CombinedOutput()
		if err != nil {
			fpBinErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if fpBinErr != nil {
		t.Fatal(fpBinErr)
	}
	dir := t.TempDir()
	addr := freeAddr(t)
	reclaim := `{idleAfter: 30s}` // no warm tier: idle machines are deleted
	if tgt.parks {
		reclaim = `{idleAfter: 30s, deleteAfter: 60s}`
	}
	cfg := fmt.Sprintf(`
server: { addr: %q, opsAddr: %q, shutdownGrace: 2s }
storage: { path: %s }
engine: { pollInterval: 2s }
reconcile: { interval: 5s }
providers:
  %s: %s
classes:
  e2e: { kind: compute.machine, provider: %s, spec: %s, reclaim: %s }
  e2e-pool: { kind: compute.machine, provider: %s, spec: %s }
`, addr, freeAddr(t), filepath.Join(dir, "fp.db"), tgt.name, tgt.provider,
		tgt.name, tgt.machineSpec(), reclaim, tgt.name, tgt.machineSpec())
	s := &fpServer{t: t, base: "http://" + addr, cfgPath: filepath.Join(dir, "config.yaml")}
	if err := os.WriteFile(s.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	s.c = apiclient.New(s.base, "")
	s.start()
	t.Cleanup(func() {
		s.kill()
		if t.Failed() {
			log, _ := os.ReadFile(s.cfgPath + ".log")
			t.Logf("fleetplane log tail:\n%s", tail(log, 6000))
		}
	})
	return s
}

func (s *fpServer) start() {
	s.t.Helper()
	logPath := s.cfgPath + ".log"
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	s.cmd = exec.Command(fpBin, "serve", "--config", s.cfgPath, "--log-level", "info")
	s.cmd.Stdout, s.cmd.Stderr = logf, logf
	if err := s.cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.stopOnce = sync.Once{}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(s.base + "/health/ready"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.kill()
	log, _ := os.ReadFile(logPath)
	s.t.Fatalf("fleetplane never became ready; log:\n%s", tail(log, 4000))
}

// kill is SIGKILL: no shutdown hooks, no flush — a crash.
func (s *fpServer) kill() {
	s.stopOnce.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
			_, _ = s.cmd.Process.Wait()
		}
	})
}

func (s *fpServer) crashAndRestart() {
	s.t.Helper()
	s.kill()
	s.start()
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// eventually polls cond every 3s until it holds or the bound trips; cond
// returns a status line for the failure message.
func eventually(t *testing.T, bound time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(bound)
	last := ""
	for {
		ok, status := cond()
		if ok {
			return
		}
		last = status
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s; last: %s", bound, what, last)
		}
		time.Sleep(3 * time.Second)
	}
}

func phaseOf(s *fpServer, id string) string {
	r, err := s.c.GetResource(context.Background(), id)
	var apiErr *apiclient.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		return "gone"
	case err != nil:
		return "error: " + err.Error()
	case r.Metadata.DeletedAt != nil:
		return "gone"
	}
	return r.Status.Phase
}

func (s *fpServer) waitPhase(t *testing.T, tgt *cloudTarget, id, want string) {
	t.Helper()
	eventually(t, tgt.wait, id+" → "+want, func() (bool, string) {
		p := phaseOf(s, id)
		return p == want, p
	})
}

func (s *fpServer) acquire(t *testing.T, tgt *cloudTarget, key string) (acqID, resID string) {
	t.Helper()
	a, err := s.c.Acquire(context.Background(), apiclient.AcquireRequest{
		Class: "e2e", Lease: &apiclient.LeaseRequest{TTL: "30m"},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, tgt.wait, "acquisition "+a.ID+" bound", func() (bool, string) {
		got, err := s.c.GetAcquisition(context.Background(), a.ID)
		if err != nil {
			return false, err.Error()
		}
		resID = got.ResourceID
		if got.State == "failed" {
			evs, _ := s.c.ListEvents(context.Background(), "", "", got.ResourceID, 50)
			var trail []string
			for _, e := range evs {
				op := ""
				if e.OperationID != "" {
					o, _ := s.c.GetOperation(context.Background(), e.OperationID)
					op = fmt.Sprintf("%+v", o)
				}
				trail = append(trail, fmt.Sprintf("%s %s %s", e.Type, e.Outcome, op))
			}
			t.Fatalf("acquisition %s failed: %+v\nevents:\n%s", a.ID, got, strings.Join(trail, "\n"))
		}
		return got.State == "bound", got.State
	})
	return a.ID, resID
}

// oneMachine asserts the cloud holds exactly one machine of this run, that
// it belongs to resID and is in phase want.
func (tgt *cloudTarget) oneMachine(t *testing.T, resID string, want provider.ObservedPhase) provider.ObservedResource {
	t.Helper()
	var m provider.ObservedResource
	eventually(t, tgt.wait, fmt.Sprintf("one %s machine for %s", want, resID), func() (bool, string) {
		live := tgt.live(t)
		if len(live) > 1 {
			t.Fatalf("DUPLICATE: cloud holds %d machines for one resource: %v", len(live), summarize(live))
		}
		if len(live) == 1 {
			m = live[0]
		}
		return len(live) == 1 && m.FleetplaneID == resID && m.Phase == want, summarize(live)
	})
	return m
}

func (tgt *cloudTarget) noMachines(t *testing.T) {
	t.Helper()
	eventually(t, tgt.wait, "cloud empty", func() (bool, string) {
		live := tgt.live(t)
		return len(live) == 0, summarize(live)
	})
}

func summarize(list []provider.ObservedResource) string {
	parts := make([]string, len(list))
	for i, m := range list {
		parts[i] = fmt.Sprintf("%s(%s,%s,%s)", m.Ref.ID, m.FleetplaneID, m.Phase, m.ProviderState)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func wantStatus(t *testing.T, err error, status int, what string) {
	t.Helper()
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != status {
		t.Fatalf("%s: got %v, want HTTP %d", what, err, status)
	}
}

// --- scenarios ---

// One machine through its whole poolless life: create on acquire, reuse on
// re-acquire, protect gates delete, drain/undrain returns it to service,
// idle parks it (stop) and the next acquire resumes the SAME machine,
// and reclaim finally deletes it. Every step is checked in the cloud.
func TestE2E_Cloud_LeaseLifecycle(t *testing.T) {
	eachCloud(t, func(t *testing.T, tgt *cloudTarget, s *fpServer) {
		ctx := context.Background()

		acq1, resID := s.acquire(t, tgt, "job-1")
		first := tgt.oneMachine(t, resID, provider.PhaseRunning)

		if err := s.c.Release(ctx, acq1); err != nil {
			t.Fatal(err)
		}
		acq2, again := s.acquire(t, tgt, "job-2")
		if again != resID {
			t.Fatalf("re-acquire bound %s, want the idle %s (reuse)", again, resID)
		}
		tgt.oneMachine(t, resID, provider.PhaseRunning)

		if err := s.c.ProtectResource(ctx, resID); err != nil {
			t.Fatal(err)
		}
		wantStatus(t, s.c.DeleteResource(ctx, resID, ""), http.StatusConflict, "delete of a protected machine")
		if err := s.c.UnprotectResource(ctx, resID); err != nil {
			t.Fatal(err)
		}

		if err := s.c.Release(ctx, acq2); err != nil {
			t.Fatal(err)
		}
		if err := s.c.DrainResource(ctx, resID); err != nil {
			t.Fatal(err)
		}
		s.waitPhase(t, tgt, resID, "draining")
		if err := s.c.UndrainResource(ctx, resID); err != nil {
			t.Fatal(err)
		}
		s.waitPhase(t, tgt, resID, "ready")
		acq3, again := s.acquire(t, tgt, "job-3")
		if again != resID {
			t.Fatalf("acquire after undrain bound %s, want %s", again, resID)
		}
		live, err := s.c.ListAcquisitions(ctx, nil, resID)
		if err != nil || len(live.Items) != 1 || live.Items[0].ID != acq3 {
			t.Fatalf("live acquisitions for %s = %+v (%v), want only %s", resID, live, err, acq3)
		}
		if err := s.c.Release(ctx, acq3); err != nil {
			t.Fatal(err)
		}

		if tgt.parks {
			s.waitPhase(t, tgt, resID, "parked") // idleAfter → stop
			tgt.oneMachine(t, resID, provider.PhaseStopped)
			acq4, again := s.acquire(t, tgt, "job-4")
			if again != resID {
				t.Fatalf("acquire of a parked class bound %s, want the parked %s (resume)", again, resID)
			}
			if m := tgt.oneMachine(t, resID, provider.PhaseRunning); m.Ref.ID != first.Ref.ID {
				t.Fatalf("resume created %s instead of starting %s", m.Ref.ID, first.Ref.ID)
			}
			if err := s.c.Release(ctx, acq4); err != nil {
				t.Fatal(err)
			}
			s.waitPhase(t, tgt, resID, "parked")
		}
		// reclaim: idle (or parked, after deleteAfter) → delete
		eventually(t, tgt.wait+2*time.Minute, resID+" reclaimed", func() (bool, string) {
			p := phaseOf(s, resID)
			return p == "gone", p
		})
		tgt.noMachines(t)
	})
}

// A pool converges to its replicas, pause freezes it (scaling to 0 does
// nothing, a manual reconcile is refused, delete is refused while members
// exist), resume drains it to zero in the cloud, and only then delete works.
func TestE2E_Cloud_PoolLifecycle(t *testing.T) {
	eachCloud(t, func(t *testing.T, tgt *cloudTarget, s *fpServer) {
		ctx := context.Background()
		name := "e2e-pool"
		var id string
		apply := func(replicas int) string {
			p, err := s.c.ApplyPool(ctx, apiclient.PoolManifest{
				Metadata: apiclient.Metadata{Name: name},
				Spec:     []byte(fmt.Sprintf(`{"class":"e2e-pool","replicas":%d}`, replicas)),
			})
			if err != nil {
				t.Fatal(err)
			}
			return p.Metadata.ID
		}
		members := func() (ready int, status string) {
			list, err := s.c.ListResources(ctx, apiclient.ResourceFilter{Pool: id})
			if err != nil {
				return 0, err.Error()
			}
			var ph []string
			for _, r := range list.Items {
				ph = append(ph, r.Status.Phase)
				if r.Status.Phase == "ready" {
					ready++
				}
			}
			return ready, fmt.Sprint(ph)
		}
		running := func() int {
			n := 0
			for _, m := range tgt.live(t) {
				if m.Phase == provider.PhaseRunning {
					n++
				}
			}
			return n
		}

		id = apply(2)
		eventually(t, tgt.wait, "2 ready members", func() (bool, string) {
			n, st := members()
			return n == 2, st
		})
		eventually(t, tgt.wait, "2 running machines", func() (bool, string) {
			return running() == 2 && len(tgt.live(t)) == 2, summarize(tgt.live(t))
		})

		if err := s.c.PausePool(ctx, id); err != nil {
			t.Fatal(err)
		}
		apply(0)
		wantStatus(t, s.c.ReconcilePool(ctx, id), http.StatusConflict, "reconcile of a paused pool")
		time.Sleep(20 * time.Second) // four reconcile intervals
		if n, st := members(); n != 2 || len(tgt.live(t)) != 2 {
			t.Fatalf("paused pool changed: members %s, cloud %s", st, summarize(tgt.live(t)))
		}
		wantStatus(t, s.c.DeletePool(ctx, id), http.StatusConflict, "delete of a pool with members")

		if err := s.c.ResumePool(ctx, id); err != nil {
			t.Fatal(err)
		}
		eventually(t, tgt.wait, "pool drained to 0", func() (bool, string) {
			list, err := s.c.ListResources(ctx, apiclient.ResourceFilter{Pool: id})
			if err != nil {
				return false, err.Error()
			}
			return len(list.Items) == 0, fmt.Sprint(len(list.Items))
		})
		tgt.noMachines(t)
		if err := s.c.DeletePool(ctx, id); err != nil {
			t.Fatalf("delete of an empty pool: %v", err)
		}
		_, err := s.c.GetPool(ctx, id)
		wantStatus(t, err, http.StatusNotFound, "get of a deleted pool")
	})
}

// Invariant 7 through a real process boundary: SIGKILL right after the
// create is journaled, and again once the cloud holds the machine but the
// control plane has not yet seen it succeed. Recovery must adopt that one
// machine — never create a second.
func TestE2E_Cloud_CrashNeverDuplicates(t *testing.T) {
	eachCloud(t, func(t *testing.T, tgt *cloudTarget, s *fpServer) {
		ctx := context.Background()
		res, err := s.c.CreateResource(ctx, apiclient.CreateResourceRequest{
			Metadata: apiclient.Metadata{Name: "crash"},
			Spec:     apiclient.ResourceSpec{Class: "e2e"},
		}, "crash-1")
		if err != nil {
			t.Fatal(err)
		}
		id := res.Metadata.ID
		s.crashAndRestart() // crash 1: 202 returned ⇒ journaled; dispatch may or may not have run

		eventually(t, tgt.wait, "machine visible in the cloud", func() (bool, string) {
			live := tgt.live(t)
			return len(live) >= 1, summarize(live)
		})
		t.Logf("crash 2 with the resource in phase %q (provisioning = cloud accepted, not yet succeeded)", phaseOf(s, id))
		s.crashAndRestart() // crash 2: the cloud has it; the journal may not know it succeeded

		s.waitPhase(t, tgt, id, "ready")
		m := tgt.oneMachine(t, id, provider.PhaseRunning)
		r, err := s.c.GetResource(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status.ExternalID != m.Ref.ID {
			t.Fatalf("resource points at %s, cloud machine is %s", r.Status.ExternalID, m.Ref.ID)
		}
		if err := s.c.DeleteResource(ctx, id, ""); err != nil {
			t.Fatal(err)
		}
		s.waitPhase(t, tgt, id, "gone")
		tgt.noMachines(t)
	})
}
