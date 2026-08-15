package tests

// Workload readiness gate (plan R16, 08 §4): provider-level success does
// not mean the workload inside the VM is up; classes may declare a TCP
// probe that gates provisioning → ready.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"testing"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/providers/fake"
)

func probedSpec(port int, budget string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"serverType":"cpx31","image":"snapshot:ci=1","readiness":{"tcp":{"port":%d},"budget":%q}}`,
		port, budget))
}

func createWithSpec(t *testing.T, h *Harness, name string, spec json.RawMessage) storage.ResourceID {
	t.Helper()
	out, err := h.Svc.CreateResource(context.Background(), app.CreateResourceCmd{
		Kind: "compute.machine", Provider: "fake-local", Name: name,
		Spec: spec, Actor: "test",
		BuildResponse: func(r *storage.Resource) (int, json.RawMessage) {
			b, _ := json.Marshal(map[string]string{"id": string(r.ID)})
			return 201, b
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ ID string }
	_ = json.Unmarshal(out.Body, &body)
	return storage.ResourceID(body.ID)
}

func TestReadiness_TCPProbeGatesReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	h := newHarness(t, fake.Options{})
	resID := createWithSpec(t, h, "probed", probedSpec(port, "5m"))
	h.Drive(resID)

	res, err := h.St.Resources().Get(context.Background(), resID)
	if err != nil || res.Phase != phase.Ready {
		t.Fatalf("probed resource: %+v %v (fake answers on 127.0.0.1)", res, err)
	}
}

func TestReadiness_BudgetExhaustedFailsResource(t *testing.T) {
	h := newHarness(t, fake.Options{})
	// Port 1: nothing listens; tiny budget. Drive advances the clock 7s
	// per step, so the budget exhausts quickly and the op must FAIL — a
	// "ready" VM whose workload never comes up is a lie (plan R16).
	resID := createWithSpec(t, h, "unreachable", probedSpec(1, "10s"))
	h.Drive(resID)

	res, err := h.St.Resources().Get(context.Background(), resID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != phase.Failed {
		t.Fatalf("phase = %s, want failed after probe budget exhaustion", res.Phase)
	}
}
