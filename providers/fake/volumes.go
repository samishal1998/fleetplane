package fake

// The fake's storage.volume driver — the Phase-9 genericity proof: a second,
// deliberately non-VM kind flowing through the SAME kernel unchanged.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/samimishal/fleetplane/pkg/kinds/volume"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

type volObject struct {
	id     string
	name   string
	state  string // available | gone
	spec   volume.Spec
	labels map[string]string
}

type fakeVolumes struct{ f *Fake }

func (v *fakeVolumes) Kind() provider.ResourceKind { return volume.Kind }

func (v *fakeVolumes) Discover(_ context.Context, req provider.DiscoverRequest) ([]provider.ObservedResource, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []provider.ObservedResource
	for _, o := range f.volumes {
		if o.state == "gone" {
			continue
		}
		owned := o.labels[provider.LabelManaged] == "true" && o.labels[provider.LabelOwner] == f.ownerID
		if req.Scope != provider.ScopeAll && !owned {
			continue
		}
		if !labelsMatch(o.labels, req.Selector) {
			continue
		}
		out = append(out, v.observeLocked(o))
	}
	return out, nil
}

func (v *fakeVolumes) Get(_ context.Context, ref provider.ExternalRef) (provider.ObservedResource, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.volumes[ref.ID]
	if !ok || o.state == "gone" {
		return provider.ObservedResource{}, f.notFound(ref.ID)
	}
	return v.observeLocked(o), nil
}

type volCreateParams struct {
	Name   string            `json:"name"`
	Spec   volume.Spec       `json:"spec"`
	Labels map[string]string `json:"labels"`
}

func (v *fakeVolumes) Plan(_ context.Context, req provider.PlanRequest) (provider.Plan, error) {
	switch {
	case req.Desired != nil && req.Observed == nil:
		spec, err := volume.ParseSpec(req.Desired.Spec)
		if err != nil {
			return provider.Plan{}, &provider.Error{Class: provider.ErrInvalid,
				SideEffect: provider.EffectNone, Provider: v.f.instance, Message: err.Error()}
		}
		params, err := json.Marshal(volCreateParams{Name: req.Desired.Name, Spec: *spec, Labels: req.Desired.Labels})
		if err != nil {
			return provider.Plan{}, err
		}
		return provider.Plan{
			Actions: []provider.Action{{Kind: "create", ResourceID: req.ResourceID, Params: params}},
			Summary: []string{fmt.Sprintf("create volume %q (%d GiB)", req.Desired.Name, spec.SizeGiB)},
		}, nil
	case req.Desired == nil && req.Observed != nil:
		return provider.Plan{
			Actions: []provider.Action{{Kind: "delete", ResourceID: req.ResourceID, Ref: &req.Observed.Ref, Destructive: true}},
			Summary: []string{"delete volume " + req.Observed.Ref.ID},
		}, nil
	default:
		return provider.Plan{}, nil
	}
}

func (v *fakeVolumes) Apply(_ context.Context, action provider.Action) (provider.OperationRef, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	switch action.Kind {
	case "create":
		var p volCreateParams
		if err := json.Unmarshal(action.Params, &p); err != nil {
			return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
				SideEffect: provider.EffectNone, Provider: f.instance, Message: "malformed volume params: " + err.Error()}
		}
		// Op-label dedup (the invariant-7 anchor, kind-independent).
		for _, o := range f.volumes {
			if o.state != "gone" && o.labels[provider.LabelOp] == action.ActionID {
				return provider.OperationRef{ActionID: action.ActionID, Ref: &provider.ExternalRef{ID: o.id}}, nil
			}
		}
		f.seq++
		id := "vol-" + strconv.Itoa(9000+f.seq)
		f.volumes[id] = &volObject{id: id, name: p.Name, state: "available", spec: p.Spec, labels: p.Labels}
		return provider.OperationRef{ActionID: action.ActionID, Ref: &provider.ExternalRef{ID: id}}, nil
	case "delete":
		if action.Ref == nil {
			return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
				SideEffect: provider.EffectNone, Provider: f.instance, Message: "delete requires a ref"}
		}
		if o, ok := f.volumes[action.Ref.ID]; ok {
			o.state = "gone"
		}
		return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref}, nil
	default:
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: f.instance, Message: "unknown action kind " + action.Kind}
	}
}

func (v *fakeVolumes) ObserveOperation(_ context.Context, op provider.OperationRef) (provider.OperationStatus, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if op.Ref == nil {
		return provider.OperationStatus{State: provider.OpUnknown}, nil
	}
	o, ok := f.volumes[op.Ref.ID]
	if !ok || o.state == "gone" {
		return provider.OperationStatus{}, f.notFound(op.Ref.ID)
	}
	obs := v.observeLocked(o)
	return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref, Resource: &obs}, nil
}

func (v *fakeVolumes) observeLocked(o *volObject) provider.ObservedResource {
	f := v.f
	labels := make(map[string]string, len(o.labels))
	for k, val := range o.labels {
		labels[k] = val
	}
	ext, _ := json.Marshal(map[string]any{"fake": true, "volume": o.name, "filesystem": o.spec.Filesystem})
	ph := provider.PhaseRunning
	if o.state == "gone" {
		ph = provider.PhaseGone
	}
	return provider.ObservedResource{
		Ref:           provider.ExternalRef{ID: o.id},
		Kind:          volume.Kind,
		FleetplaneID:  labels[provider.LabelID],
		CreateOpID:    labels[provider.LabelOp],
		Owned:         labels[provider.LabelManaged] == "true" && labels[provider.LabelOwner] == f.ownerID,
		Phase:         ph,
		ProviderState: o.state,
		Capacity:      provider.Capacity{volume.DimStorageGiB: o.spec.SizeGiB},
		Labels:        labels,
		Extensions:    ext,
		ObservedAt:    time.Now(),
	}
}
