package tests

// Custom machines and the control-VM bootstrap, on real clouds through the
// real binary (see e2e_cloud_test.go for the harness).

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	gce "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/samishal1998/fleetplane/internal/api"
	"github.com/samishal1998/fleetplane/pkg/apiclient"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

// markerUserData makes cloud-init observable from the cloud API alone: it
// installs a unit that powers the machine off ~45s after EVERY boot, then
// powers this first boot off. A machine that stops proves its user-data
// ran; a clone that stops proves it booted from the snapshotted disk.
const markerUserData = `#cloud-config
write_files:
  - path: /etc/systemd/system/fp-e2e-poweroff.service
    permissions: "0644"
    content: |
      [Unit]
      Description=fleetplane e2e: prove this disk booted
      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'sleep 45; systemctl poweroff'
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [systemctl, enable, fp-e2e-poweroff.service]
power_state:
  mode: poweroff
  delay: now
`

// Custom machines: cloud-init user-data runs (the source machine powers
// itself off), and a machine created from a "snapshot:<label>" image
// selector boots that snapshot (the clone powers itself off with no
// user-data at all). Image families are covered by the GCP conformance run.
func TestE2E_Cloud_CustomImage(t *testing.T) {
	eachCloud(t, func(t *testing.T, tgt *cloudTarget, s *fpServer) {
		if tgt.snapshot == nil || tgt.ubuntu == "" {
			t.Skip("target has no cloud-init / snapshots")
		}
		ctx := context.Background()

		src := s.createInline(t, tgt, "image-source", tgt.machineWith(tgt.ubuntu, markerUserData))
		s.waitPhase(t, tgt, src, "ready")
		booted := tgt.waitMachinePhase(t, src, provider.PhaseRunning, "booted")
		m := tgt.waitMachinePhase(t, src, provider.PhaseStopped, "cloud-init power_state ran")
		if m.Ref.ID != booted.Ref.ID {
			t.Fatalf("source machine changed identity: %s → %s", booted.Ref.ID, m.Ref.ID)
		}

		selector := tgt.snapshot(t, m.Ref, tgt.runID)
		if err := s.c.DeleteResource(ctx, src, ""); err != nil {
			t.Fatal(err)
		}

		clone := s.createInline(t, tgt, "image-clone", tgt.machineWith(selector, ""))
		s.waitPhase(t, tgt, clone, "ready")
		// Running first: a clone that never powered on must not pass as
		// "it powered itself off".
		cm := tgt.waitMachinePhase(t, clone, provider.PhaseRunning, "clone booted")
		if cm.Ref.ID == m.Ref.ID {
			t.Fatal("clone resolved to the source machine")
		}
		tgt.waitMachinePhase(t, clone, provider.PhaseStopped, "the snapshot's boot unit ran")

		if err := s.c.DeleteResource(ctx, clone, ""); err != nil {
			t.Fatal(err)
		}
		s.waitPhase(t, tgt, src, "gone")
		s.waitPhase(t, tgt, clone, "gone")
		tgt.noMachines(t)
	})
}

func hetznerSnapshot(t *testing.T, ref provider.ExternalRef, runID string) string {
	t.Helper()
	ctx := context.Background()
	client := hcloud.NewClient(hcloud.WithToken(os.Getenv("HETZNER_TOKEN")))
	id, err := strconv.ParseInt(ref.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	res, _, err := client.Server.CreateImage(ctx, &hcloud.Server{ID: id}, &hcloud.ServerCreateImageOpts{
		Type: hcloud.ImageTypeSnapshot, Description: hcloud.Ptr("fleetplane e2e " + runID),
		Labels: map[string]string{"fleetplane.io/test": "1", "fp-e2e-run": runID},
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Cleanup(func() {
		if _, err := client.Image.Delete(context.Background(), res.Image); err != nil {
			t.Errorf("delete snapshot %d: %v", res.Image.ID, err)
		}
	})
	wctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := client.Action.WaitFor(wctx, res.Action); err != nil {
		t.Fatalf("snapshot action: %v", err)
	}
	return "snapshot:fp-e2e-run=" + runID
}

func gcpSnapshot(t *testing.T, ref provider.ExternalRef, runID string) string {
	t.Helper()
	ctx := context.Background()
	project := os.Getenv("GCP_E2E_PROJECT")
	svc, err := gce.NewService(ctx,
		option.WithAuthCredentialsFile(option.ServiceAccount, os.Getenv("GCP_E2E_CREDENTIALS_FILE")),
		option.WithUserAgent("fleetplane-e2e"))
	if err != nil {
		t.Fatal(err)
	}
	inst, err := svc.Instances.Get(project, ref.Extra["zone"], ref.ID).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	name := "fp-e2e-" + runID
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	if _, err := svc.Images.Insert(project, &gce.Image{
		Name: name, SourceDisk: inst.Disks[0].Source,
		Labels: map[string]string{"fp-test": "1", "fp-e2e-run": runID},
	}).Context(ctx).Do(); err != nil {
		t.Fatalf("image insert: %v", err)
	}
	t.Cleanup(func() {
		if _, err := svc.Images.Delete(project, name).Context(context.Background()).Do(); err != nil {
			t.Errorf("delete image %s: %v", name, err)
		}
	})
	eventually(t, 10*time.Minute, "image "+name+" READY", func() (bool, string) {
		img, err := svc.Images.Get(project, name).Context(ctx).Do()
		if err != nil {
			return false, err.Error()
		}
		return img.Status == "READY", img.Status
	})
	return "snapshot:fp-e2e-run=" + runID
}

// The SETUP_GUIDE §4 path, end to end: `fleetplane cloud-init` renders
// user-data, the local control plane boots a Hetzner VM with it, and the
// VM comes up as a working control plane — reachable from here with the
// token the renderer was given, and able to provision. It installs the
// RELEASED binary (FLEETPLANE_E2E_BOOTSTRAP_VERSION, default v0.8.0), not
// this branch. The remote control plane drives the fake provider, so no
// real credential ever lands in user-data.
func TestE2E_Cloud_ControlVMBootstrap(t *testing.T) {
	eachCloud(t, func(t *testing.T, tgt *cloudTarget, s *fpServer) {
		if tgt.name != "hetzner-e2e" {
			// GCP's default network firewalls :8080; docker has no cloud-init.
			t.Skip("bootstrap runs on Hetzner only")
		}
		ctx := context.Background()
		set, _ := api.NewPermSet([]string{"admin"})
		token, _, err := api.GenerateToken("e2e", set)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		cfg := filepath.Join(dir, "control.yaml")
		if err := os.WriteFile(cfg, []byte(`
server: { addr: ":8080" }
storage: { path: /var/lib/fleetplane/fp.db }
providers:
  fake-local: { driver: fake }
classes:
  small: { kind: compute.machine, provider: fake-local, spec: { serverType: small, image: "name:any" } }
`), 0o600); err != nil {
			t.Fatal(err)
		}
		ud := filepath.Join(dir, "user-data.yaml")
		out, err := exec.Command(fpBin, "cloud-init", "--config", cfg, "--token", token,
			"--version", envOr("FLEETPLANE_E2E_BOOTSTRAP_VERSION", "v0.8.0"), "--out", ud).CombinedOutput()
		if err != nil {
			t.Fatalf("fleetplane cloud-init: %v\n%s", err, out)
		}
		userData, err := os.ReadFile(ud)
		if err != nil {
			t.Fatal(err)
		}

		vm := s.createInline(t, tgt, "control-vm", tgt.machineWith(tgt.ubuntu, string(userData)))
		s.waitPhase(t, tgt, vm, "ready")
		m, _ := tgt.machineOf(t, vm)
		ip := ""
		for _, a := range m.Addresses {
			if a.Network == "public-v4" {
				ip = a.Addr
			}
		}
		if ip == "" {
			t.Fatalf("control VM has no public IPv4: %+v", m.Addresses)
		}

		remote := apiclient.New("http://"+ip+":8080", token)
		eventually(t, 10*time.Minute, "remote control plane answering /v1 with the rendered token", func() (bool, string) {
			_, err := remote.ListResources(ctx, apiclient.ResourceFilter{})
			if err != nil {
				return false, err.Error()
			}
			return true, ""
		})
		// Auth is really on: no token → 401.
		_, err = apiclient.New("http://"+ip+":8080", "").ListResources(ctx, apiclient.ResourceFilter{})
		wantStatus(t, err, http.StatusUnauthorized, "remote list without a token")

		res, err := remote.CreateResource(ctx, apiclient.CreateResourceRequest{
			Metadata: apiclient.Metadata{Name: "via-control-vm"}, Spec: apiclient.ResourceSpec{Class: "small"},
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, 2*time.Minute, "remote resource ready", func() (bool, string) {
			r, err := remote.GetResource(ctx, res.Metadata.ID)
			if err != nil {
				return false, err.Error()
			}
			return r.Status.Phase == "ready", r.Status.Phase
		})
		t.Logf("control VM %s at %s serves the API and provisions", m.Ref.ID, ip)

		if err := s.c.DeleteResource(ctx, vm, ""); err != nil {
			t.Fatal(err)
		}
		s.waitPhase(t, tgt, vm, "gone")
		tgt.noMachines(t)
	})
}
