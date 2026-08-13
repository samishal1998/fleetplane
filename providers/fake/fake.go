// Package fake is the in-memory provider: the kernel's primary test double
// and the substrate for the failure-injection catalog (plan R13).
//
// Design: single mutex, zero goroutines, step-clocked — asynchronous
// progress advances per ObserveOperation/Discover CALL, never wall time, so
// every test is deterministic. I2 ships the synchronous skeleton; step
// counts, fault rules and list-lag land with the conformance increment.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/samimishal/fleetplane/pkg/kinds/compute"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

// Driver is the registry name.
const Driver = "fake"

func init() {
	provider.Register(Driver, func(_ context.Context, cfg provider.InstanceConfig) (provider.Provider, error) {
		return New(cfg.Instance, cfg.OwnerID), nil
	})
}

// Fake implements provider.Provider and the compute.machine driver.
type Fake struct {
	mu       sync.Mutex
	instance string
	ownerID  string
	seq      int
	objects  map[string]*object // by external ID
}

type object struct {
	id       string
	name     string
	state    string // "running" | "gone"
	spec     compute.MachineSpec
	labels   map[string]string
	capacity provider.Capacity
}

// New builds an unregistered Fake (tests construct directly; production
// config goes through provider.New).
func New(instance, ownerID string) *Fake {
	return &Fake{instance: instance, ownerID: ownerID, objects: map[string]*object{}}
}

// --- provider.Provider ---

func (f *Fake) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Driver:                 Driver,
		Instance:               f.instance,
		Version:                "dev",
		Kinds:                  []provider.ResourceKind{compute.Kind},
		SupportsLabelDiscovery: true,
	}
}

func (f *Fake) Capabilities(context.Context) ([]provider.CapabilityID, error) {
	return []provider.CapabilityID{"compute.machine.create"}, nil
}

func (f *Fake) ResourceDriver(kind provider.ResourceKind) (provider.ResourceDriver, bool) {
	if kind != compute.Kind {
		return nil, false
	}
	return f, true
}

func (f *Fake) Health(context.Context) error { return nil }
func (f *Fake) Close() error                 { return nil }

// --- provider.ResourceDriver ---

func (f *Fake) Kind() provider.ResourceKind { return compute.Kind }

func (f *Fake) Discover(_ context.Context, req provider.DiscoverRequest) ([]provider.ObservedResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []provider.ObservedResource
	for _, o := range f.objects {
		if o.state == "gone" {
			continue
		}
		if req.Scope != provider.ScopeAll && !f.ownedLocked(o) {
			continue
		}
		if !labelsMatch(o.labels, req.Selector) {
			continue
		}
		out = append(out, f.observeLocked(o))
	}
	return out, nil
}

func (f *Fake) Get(_ context.Context, ref provider.ExternalRef) (provider.ObservedResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref.ID]
	if !ok || o.state == "gone" {
		return provider.ObservedResource{}, f.notFound(ref.ID)
	}
	return f.observeLocked(o), nil
}

func (f *Fake) Plan(_ context.Context, req provider.PlanRequest) (provider.Plan, error) {
	switch {
	case req.Desired != nil && req.Observed == nil:
		spec, err := compute.ParseSpec(req.Desired.Spec)
		if err != nil {
			return provider.Plan{}, &provider.Error{
				Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: f.instance, Message: err.Error(),
			}
		}
		params, err := json.Marshal(createParams{Name: req.Desired.Name, Spec: *spec, Labels: req.Desired.Labels})
		if err != nil {
			return provider.Plan{}, err
		}
		return provider.Plan{
			Actions: []provider.Action{{
				Kind:       "create",
				ResourceID: req.ResourceID,
				Params:     params,
			}},
			Summary: []string{fmt.Sprintf("create machine %q (%s)", req.Desired.Name, spec.ServerType)},
		}, nil
	case req.Desired == nil && req.Observed != nil:
		return provider.Plan{
			Actions: []provider.Action{{
				Kind:        "delete",
				ResourceID:  req.ResourceID,
				Ref:         &req.Observed.Ref,
				Destructive: true,
			}},
			Summary: []string{fmt.Sprintf("delete machine %s", req.Observed.Ref.ID)},
		}, nil
	default:
		return provider.Plan{}, nil // converged or nothing to do
	}
}

type createParams struct {
	Name   string              `json:"name"`
	Spec   compute.MachineSpec `json:"spec"`
	Labels map[string]string   `json:"labels"`
}

func (f *Fake) Apply(_ context.Context, action provider.Action) (provider.OperationRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch action.Kind {
	case "create":
		var p createParams
		if err := json.Unmarshal(action.Params, &p); err != nil {
			return provider.OperationRef{}, &provider.Error{
				Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: f.instance, Message: "malformed create params: " + err.Error(),
			}
		}
		// Idempotent by op label (defense in depth): a create whose
		// ActionID already produced an object returns the same ref.
		for _, o := range f.objects {
			if o.state != "gone" && o.labels[provider.LabelOp] == action.ActionID {
				return provider.OperationRef{ActionID: action.ActionID, Ref: &provider.ExternalRef{ID: o.id}}, nil
			}
		}
		f.seq++
		id := strconv.Itoa(100000 + f.seq)
		labels := map[string]string{}
		for k, v := range p.Spec.Labels {
			labels[k] = v
		}
		for k, v := range p.Labels {
			labels[k] = v
		}
		f.objects[id] = &object{
			id: id, name: p.Name, state: "running", spec: p.Spec, labels: labels,
			capacity: provider.Capacity{compute.DimCPU: 2, compute.DimMemoryMiB: 4096},
		}
		return provider.OperationRef{ActionID: action.ActionID, Ref: &provider.ExternalRef{ID: id}}, nil

	case "delete":
		if action.Ref == nil {
			return provider.OperationRef{}, &provider.Error{
				Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: f.instance, Message: "delete requires a ref",
			}
		}
		o, ok := f.objects[action.Ref.ID]
		if !ok || o.state == "gone" {
			// Delete of already-deleted is success (docs/03 §6 contract).
			return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref}, nil
		}
		o.state = "gone"
		return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref}, nil

	default:
		return provider.OperationRef{}, &provider.Error{
			Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: f.instance, Message: fmt.Sprintf("unknown action kind %q", action.Kind),
		}
	}
}

func (f *Fake) ObserveOperation(_ context.Context, op provider.OperationRef) (provider.OperationStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if op.Ref == nil {
		return provider.OperationStatus{State: provider.OpUnknown}, nil
	}
	o, ok := f.objects[op.Ref.ID]
	if !ok || o.state == "gone" {
		// For deletes this is success; the engine interprets by op kind.
		return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref}, nil
	}
	obs := f.observeLocked(o)
	return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref, Resource: &obs}, nil
}

// --- helpers ---

func (f *Fake) ownedLocked(o *object) bool {
	return o.labels[provider.LabelManaged] == "true" && o.labels[provider.LabelOwner] == f.ownerID
}

func (f *Fake) observeLocked(o *object) provider.ObservedResource {
	labels := make(map[string]string, len(o.labels))
	for k, v := range o.labels {
		labels[k] = v
	}
	ext, _ := json.Marshal(map[string]any{"fake": true, "name": o.name, "serverType": o.spec.ServerType})
	ph := provider.PhaseRunning
	if o.state == "gone" {
		ph = provider.PhaseGone
	}
	return provider.ObservedResource{
		Ref:           provider.ExternalRef{ID: o.id},
		Kind:          compute.Kind,
		FleetplaneID:  labels[provider.LabelID],
		CreateOpID:    labels[provider.LabelOp],
		Owned:         f.ownedLocked(o),
		Phase:         ph,
		ProviderState: o.state,
		Capacity:      o.capacity,
		Addresses:     []provider.Address{{Network: "public-v4", Addr: "127.0.0.1"}},
		Labels:        labels,
		Extensions:    ext,
		ObservedAt:    time.Now(),
	}
}

func (f *Fake) notFound(id string) *provider.Error {
	return &provider.Error{
		Class: provider.ErrNotFound, SideEffect: provider.EffectNone,
		Provider: f.instance, Message: fmt.Sprintf("machine %s not found", id),
	}
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
