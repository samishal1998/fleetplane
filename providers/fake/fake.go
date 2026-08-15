// Package fake is the in-memory provider: the kernel's primary test double
// and the substrate for the failure-injection catalog (plan R13).
//
// Design: single mutex, zero goroutines, step-clocked — asynchronous
// progress advances per ObserveOperation/Discover CALL, never wall time, so
// every test is deterministic. Fault injection is a declarative rule queue
// plus named hook points.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/kinds/volume"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

// Driver is the registry name.
const Driver = "fake"

func init() {
	provider.Register(Driver, func(_ context.Context, cfg provider.InstanceConfig) (provider.Provider, error) {
		var opt Options
		if len(cfg.Settings) > 0 && string(cfg.Settings) != "null" {
			if err := json.Unmarshal(cfg.Settings, &opt); err != nil {
				return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
					Message: "fake settings: " + err.Error()}
			}
		}
		return New(cfg.Instance, cfg.OwnerID, opt), nil
	})
}

// Options tune the deterministic async/consistency model. The zero value is
// fully synchronous (creates land running, lists see everything at once).
type Options struct {
	CreateSteps  int `json:"createSteps"`  // ObserveOperation calls until a create lands running
	DeleteSteps  int `json:"deleteSteps"`  // ObserveOperation calls until a delete lands gone
	ListLagSteps int `json:"listLagSteps"` // Discover calls before a new object becomes listable
	PageSize     int `json:"pageSize"`     // internal Discover pagination chunk (0 = single page)
}

// HookPoint names an interception point.
type HookPoint int

const (
	BeforeApply HookPoint = iota
	AfterAccept
	BeforeObserve
	BeforeDiscover
)

// HookCtx is passed to hooks; fields are set where meaningful.
type HookCtx struct {
	Action *provider.Action
	OpRef  *provider.OperationRef
}

type applyFault struct {
	class    provider.ErrorClass
	effect   provider.SideEffect
	mutate   bool // AcceptButDropResponse: perform the mutation, lose the reply
	retryIn  time.Duration
	message  string
	kindOnly string // restrict to an action kind ("" = any)
}

// Fake implements provider.Provider and the compute.machine driver.
type Fake struct {
	mu       sync.Mutex
	instance string
	ownerID  string
	opt      Options
	seq      int
	objects  map[string]*object

	volumes map[string]*volObject

	applyFaults    []applyFault
	rateLimitLeft  int
	rateRetryAfter time.Duration
	hooks          map[HookPoint][]func(HookCtx) error

	// Counters for test assertions.
	ApplyCalls, ObserveCalls, DiscoverCalls int
}

type object struct {
	id        string
	name      string
	state     string // creating | running | deleting | gone
	stepsLeft int
	listLag   int
	spec      compute.MachineSpec
	labels    map[string]string
	capacity  provider.Capacity
}

// New builds an unregistered Fake (tests construct directly; production
// config goes through provider.New).
func New(instance, ownerID string, opt Options) *Fake {
	return &Fake{
		instance: instance, ownerID: ownerID, opt: opt,
		objects: map[string]*object{},
		volumes: map[string]*volObject{},
		hooks:   map[HookPoint][]func(HookCtx) error{},
	}
}

// --- fault injection (plan R13) ---

// FailNextApply queues n Apply failures of the given class/effect.
func (f *Fake) FailNextApply(class provider.ErrorClass, effect provider.SideEffect, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 0; i < n; i++ {
		f.applyFaults = append(f.applyFaults, applyFault{
			class: class, effect: effect, message: "injected apply failure",
		})
	}
}

// AcceptButDropResponse makes the next create Apply PERFORM the mutation and
// then lose the response (FI-1): the caller sees EffectMaybe.
func (f *Fake) AcceptButDropResponse() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyFaults = append(f.applyFaults, applyFault{
		class: provider.ErrRetryable, effect: provider.EffectMaybe,
		mutate: true, message: "injected: response lost after accept", kindOnly: "create",
	})
}

// RateLimitNext makes the next n driver calls fail rate-limited (FI-5).
func (f *Fake) RateLimitNext(n int, retryAfter time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateLimitLeft = n
	f.rateRetryAfter = retryAfter
}

// SetListLagSteps overrides list visibility lag for future creates (FI-4).
func (f *Fake) SetListLagSteps(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opt.ListLagSteps = n
}

// Hook installs fn at a hook point; returning an error aborts the call.
func (f *Fake) Hook(p HookPoint, fn func(HookCtx) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[p] = append(f.hooks[p], fn)
}

// InjectRunning plants a running object directly (test scaffolding for
// ghost/adoption scenarios that cannot arise through Apply's dedup).
func (f *Fake) InjectRunning(name string, labels map[string]string) provider.ExternalRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := strconv.Itoa(100000 + f.seq)
	cp := map[string]string{}
	for k, v := range labels {
		cp[k] = v
	}
	f.objects[id] = &object{
		id: id, name: name, state: "running", labels: cp,
		spec:     compute.MachineSpec{ServerType: "cpx31", Image: "snapshot:injected"},
		capacity: provider.Capacity{compute.DimCPU: 2, compute.DimMemoryMiB: 4096},
	}
	return provider.ExternalRef{ID: id}
}

// Objects returns ground truth (every non-gone object) for assertions.
func (f *Fake) Objects() []provider.ObservedResource {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []provider.ObservedResource
	for _, o := range f.objects {
		if o.state != "gone" {
			out = append(out, f.observeLocked(o))
		}
	}
	return out
}

func (f *Fake) runHooksLocked(p HookPoint, hc HookCtx) error {
	for _, fn := range f.hooks[p] {
		if err := fn(hc); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fake) rateLimitedLocked() error {
	if f.rateLimitLeft > 0 {
		f.rateLimitLeft--
		return &provider.Error{
			Class: provider.ErrRateLimited, SideEffect: provider.EffectNone,
			Provider: f.instance, Message: "injected rate limit",
			RetryAfter: f.rateRetryAfter,
		}
	}
	return nil
}

// --- provider.Provider ---

func (f *Fake) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Driver: Driver, Instance: f.instance, Version: "dev",
		Kinds:                  []provider.ResourceKind{compute.Kind, volume.Kind},
		SupportsLabelDiscovery: true,
	}
}

func (f *Fake) Capabilities(context.Context) ([]provider.CapabilityID, error) {
	return []provider.CapabilityID{"compute.machine.create", "storage.volume.create"}, nil
}

func (f *Fake) ResourceDriver(kind provider.ResourceKind) (provider.ResourceDriver, bool) {
	switch kind {
	case compute.Kind:
		return f, true
	case volume.Kind:
		return &fakeVolumes{f: f}, true
	default:
		return nil, false
	}
}

func (f *Fake) Health(context.Context) error { return nil }
func (f *Fake) Close() error                 { return nil }

// --- provider.ResourceDriver ---

func (f *Fake) Kind() provider.ResourceKind { return compute.Kind }

func (f *Fake) Discover(_ context.Context, req provider.DiscoverRequest) ([]provider.ObservedResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DiscoverCalls++
	if err := f.runHooksLocked(BeforeDiscover, HookCtx{}); err != nil {
		return nil, err
	}
	if err := f.rateLimitedLocked(); err != nil {
		return nil, err
	}
	// One list pass = one consistency step for every lagged object.
	for _, o := range f.objects {
		if o.listLag > 0 {
			o.listLag--
		}
	}
	var all []provider.ObservedResource
	for _, o := range f.objects {
		if o.state == "gone" || o.listLag > 0 {
			continue
		}
		if req.Scope != provider.ScopeAll && !f.ownedLocked(o) {
			continue
		}
		if !labelsMatch(o.labels, req.Selector) {
			continue
		}
		all = append(all, f.observeLocked(o))
	}
	// Internal pagination: chunked assembly proves the code path the real
	// providers exercise against cloud APIs.
	if f.opt.PageSize > 0 {
		var paged []provider.ObservedResource
		for i := 0; i < len(all); i += f.opt.PageSize {
			end := min(i+f.opt.PageSize, len(all))
			paged = append(paged, all[i:end]...)
		}
		all = paged
	}
	return all, nil
}

func (f *Fake) Get(_ context.Context, ref provider.ExternalRef) (provider.ObservedResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.rateLimitedLocked(); err != nil {
		return provider.ObservedResource{}, err
	}
	o, ok := f.objects[ref.ID]
	if !ok || o.state == "gone" {
		return provider.ObservedResource{}, f.notFound(ref.ID)
	}
	// Get by ref is authoritative regardless of list lag (05 §10).
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
			Actions: []provider.Action{{Kind: "create", ResourceID: req.ResourceID, Params: params}},
			Summary: []string{fmt.Sprintf("create machine %q (%s)", req.Desired.Name, spec.ServerType)},
		}, nil
	case req.Desired == nil && req.Observed != nil:
		return provider.Plan{
			Actions: []provider.Action{{Kind: "delete", ResourceID: req.ResourceID, Ref: &req.Observed.Ref, Destructive: true}},
			Summary: []string{fmt.Sprintf("delete machine %s", req.Observed.Ref.ID)},
		}, nil
	default:
		return provider.Plan{}, nil
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
	f.ApplyCalls++
	if err := f.runHooksLocked(BeforeApply, HookCtx{Action: &action}); err != nil {
		return provider.OperationRef{}, err
	}
	if err := f.rateLimitedLocked(); err != nil {
		return provider.OperationRef{}, err
	}
	// Consume a queued fault, if it applies to this action kind.
	if len(f.applyFaults) > 0 && (f.applyFaults[0].kindOnly == "" || f.applyFaults[0].kindOnly == action.Kind) {
		fault := f.applyFaults[0]
		f.applyFaults = f.applyFaults[1:]
		if fault.mutate {
			_, _ = f.applyLocked(action) // mutation lands; response is lost
		}
		return provider.OperationRef{}, &provider.Error{
			Class: fault.class, SideEffect: fault.effect,
			Provider: f.instance, Message: fault.message, RetryAfter: fault.retryIn,
		}
	}
	ref, err := f.applyLocked(action)
	if err != nil {
		return provider.OperationRef{}, err
	}
	_ = f.runHooksLocked(AfterAccept, HookCtx{Action: &action, OpRef: &ref})
	return ref, nil
}

func (f *Fake) applyLocked(action provider.Action) (provider.OperationRef, error) {
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
		state, steps := "running", 0
		if f.opt.CreateSteps > 0 {
			state, steps = "creating", f.opt.CreateSteps
		}
		f.objects[id] = &object{
			id: id, name: p.Name, state: state, stepsLeft: steps, listLag: f.opt.ListLagSteps,
			spec: p.Spec, labels: labels,
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
			// Delete of already-deleted is success (docs/03 §6, FI-7).
			return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref}, nil
		}
		if f.opt.DeleteSteps > 0 {
			o.state, o.stepsLeft = "deleting", f.opt.DeleteSteps
		} else {
			o.state = "gone"
		}
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
	f.ObserveCalls++
	if err := f.runHooksLocked(BeforeObserve, HookCtx{OpRef: &op}); err != nil {
		return provider.OperationStatus{}, err
	}
	if err := f.rateLimitedLocked(); err != nil {
		return provider.OperationStatus{}, err
	}
	if op.Ref == nil {
		return provider.OperationStatus{State: provider.OpUnknown}, nil
	}
	o, ok := f.objects[op.Ref.ID]
	if !ok || o.state == "gone" {
		// The object is gone: surface not_found and let the ENGINE
		// interpret by op kind (delete → success; create → verify).
		return provider.OperationStatus{}, f.notFound(op.Ref.ID)
	}
	// One observation = one async step.
	switch o.state {
	case "creating":
		o.stepsLeft--
		if o.stepsLeft <= 0 {
			o.state = "running"
		} else {
			return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref}, nil
		}
	case "deleting":
		o.stepsLeft--
		if o.stepsLeft <= 0 {
			o.state = "gone"
			return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref}, nil
		}
		return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref}, nil
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
	var ph provider.ObservedPhase
	switch o.state {
	case "creating":
		ph = provider.PhasePending
	case "running":
		ph = provider.PhaseRunning
	case "deleting":
		ph = provider.PhaseDeleting
	default:
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
