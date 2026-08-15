// Package hetzner drives compute.machine on Hetzner Cloud via the official
// hcloud-go v2 SDK (docs/08). Built-in SDK retries are DISABLED — the
// operation engine owns every retry so the journal sees every attempt
// (ADR-014, invariant 7).
package hetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
	"github.com/samishal1998/fleetplane/providers/pacing"
)

const Driver = "hetzner"

func init() {
	provider.Register(Driver, func(ctx context.Context, cfg provider.InstanceConfig) (provider.Provider, error) {
		return New(ctx, cfg)
	})
}

// Settings is the driver config block (03 §4).
type Settings struct {
	Token    string  `json:"token"` // secret:// reference (07 §4)
	Location string  `json:"location,omitempty"`
	Endpoint string  `json:"endpoint,omitempty"` // test override
	RPS      float64 `json:"rps,omitempty"`
	Burst    int     `json:"burst,omitempty"`
	MaxConc  int     `json:"maxConcurrent,omitempty"`
}

type Hetzner struct {
	client   *hcloud.Client
	instance string
	ownerID  string
	location string
	pacer    *pacing.Pacer
}

func New(ctx context.Context, cfg provider.InstanceConfig) (*Hetzner, error) {
	var s Settings
	if len(cfg.Settings) > 0 && string(cfg.Settings) != "null" {
		if err := json.Unmarshal(cfg.Settings, &s); err != nil {
			return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: cfg.Instance, Message: "hetzner settings: " + err.Error()}
		}
	}
	if s.Token == "" {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "hetzner: token is required (secret:// reference)"}
	}
	token := s.Token
	if secretref.IsRef(token) {
		sec, err := cfg.Secrets.Resolve(ctx, token)
		if err != nil {
			return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: cfg.Instance, Message: err.Error()}
		}
		token = string(sec.Reveal())
	}
	opts := []hcloud.ClientOption{
		hcloud.WithToken(token),
		// The engine is the only retry authority (ADR-014); hcloud-go
		// retries are ON by default since v2.11.0.
		hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}),
		hcloud.WithApplication("fleetplane", "v1alpha1"),
	}
	if s.Endpoint != "" {
		opts = append(opts, hcloud.WithEndpoint(s.Endpoint))
	}
	return &Hetzner{
		client:   hcloud.NewClient(opts...),
		instance: cfg.Instance,
		ownerID:  cfg.OwnerID,
		location: s.Location,
		pacer:    pacing.New(s.RPS, s.Burst, s.MaxConc, nil),
	}, nil
}

// --- provider.Provider ---

func (h *Hetzner) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Driver: Driver, Instance: h.instance, Version: "hcloud-go/v2",
		Kinds:                  []provider.ResourceKind{compute.Kind},
		SupportsLabelDiscovery: true,
	}
}

func (h *Hetzner) Capabilities(context.Context) ([]provider.CapabilityID, error) {
	return []provider.CapabilityID{"compute.machine.create"}, nil
}

func (h *Hetzner) ResourceDriver(kind provider.ResourceKind) (provider.ResourceDriver, bool) {
	if kind != compute.Kind {
		return nil, false
	}
	return h, true
}

func (h *Hetzner) Health(ctx context.Context) error {
	release, err := h.pacer.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	// Cheap authenticated read.
	_, resp, err := h.client.Location.List(ctx, hcloud.LocationListOpts{ListOpts: hcloud.ListOpts{PerPage: 1}})
	h.observeResp(resp)
	if err != nil {
		return h.mapErr(err, provider.EffectNone)
	}
	return nil
}

func (h *Hetzner) Close() error { return nil }

// --- provider.ResourceDriver ---

func (h *Hetzner) Kind() provider.ResourceKind { return compute.Kind }

func (h *Hetzner) Discover(ctx context.Context, req provider.DiscoverRequest) ([]provider.ObservedResource, error) {
	release, err := h.pacer.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var parts []string
	if req.Scope != provider.ScopeAll {
		parts = append(parts,
			provider.LabelManaged+"=true",
			provider.LabelOwner+"="+h.ownerID)
	}
	for k, v := range req.Selector {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	servers, err := h.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: strings.Join(parts, ","), PerPage: 50},
	})
	if err != nil {
		return nil, h.mapErr(err, provider.EffectNone)
	}
	out := make([]provider.ObservedResource, 0, len(servers))
	for _, srv := range servers {
		out = append(out, h.observe(srv))
	}
	return out, nil
}

func (h *Hetzner) Get(ctx context.Context, ref provider.ExternalRef) (provider.ObservedResource, error) {
	id, err := strconv.ParseInt(ref.ID, 10, 64)
	if err != nil {
		return provider.ObservedResource{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: h.instance, Message: "malformed server id " + ref.ID}
	}
	release, err := h.pacer.Acquire(ctx)
	if err != nil {
		return provider.ObservedResource{}, err
	}
	defer release()
	srv, resp, err := h.client.Server.GetByID(ctx, id)
	h.observeResp(resp)
	if err != nil {
		return provider.ObservedResource{}, h.mapErr(err, provider.EffectNone)
	}
	if srv == nil {
		// hcloud-go swallows the 404 into (nil, nil) — synthesize the
		// typed error the kernel branches on (plan R23).
		return provider.ObservedResource{}, h.notFound(ref.ID)
	}
	return h.observe(srv), nil
}

func (h *Hetzner) Plan(_ context.Context, req provider.PlanRequest) (provider.Plan, error) {
	switch {
	case req.Desired != nil && req.Observed == nil:
		spec, err := compute.ParseSpec(req.Desired.Spec)
		if err != nil {
			return provider.Plan{}, &provider.Error{Class: provider.ErrInvalid,
				SideEffect: provider.EffectNone, Provider: h.instance, Message: err.Error()}
		}
		params, err := json.Marshal(createParams{Name: req.Desired.Name, Spec: *spec, Labels: req.Desired.Labels})
		if err != nil {
			return provider.Plan{}, err
		}
		return provider.Plan{
			Actions: []provider.Action{{Kind: "create", ResourceID: req.ResourceID, Params: params}},
			Summary: []string{fmt.Sprintf("create hetzner server %q (%s, %s)", req.Desired.Name, spec.ServerType, spec.Image)},
		}, nil
	case req.Desired == nil && req.Observed != nil:
		return provider.Plan{
			Actions: []provider.Action{{Kind: "delete", ResourceID: req.ResourceID, Ref: &req.Observed.Ref, Destructive: true}},
			Summary: []string{"delete hetzner server " + req.Observed.Ref.ID},
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

// opData is OperationRef.Data (self-versioned; drivers tolerate nil).
type opData struct {
	V        int     `json:"v"`
	ServerID int64   `json:"serverId"`
	Delete   bool    `json:"delete,omitempty"`
	Actions  []int64 `json:"actions,omitempty"`
}

func (h *Hetzner) Apply(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	switch action.Kind {
	case "create":
		return h.applyCreate(ctx, action)
	case "delete":
		return h.applyDelete(ctx, action)
	default:
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: h.instance, Message: "unknown action kind " + action.Kind}
	}
}

func (h *Hetzner) applyCreate(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	var p createParams
	if err := json.Unmarshal(action.Params, &p); err != nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: h.instance, Message: "malformed create params: " + err.Error()}
	}

	// Op-label dedup (conformance Idempotency/OpLabelDedup): a replayed
	// create returns the server the SAME operation already produced.
	if existing, err := h.Discover(ctx, provider.DiscoverRequest{
		Scope: provider.ScopeOwned, Selector: map[string]string{provider.LabelOp: action.ActionID},
	}); err == nil && len(existing) > 0 {
		return provider.OperationRef{ActionID: action.ActionID, Ref: &existing[0].Ref}, nil
	}

	serverType, err := h.serverType(ctx, p.Spec.ServerType)
	if err != nil {
		return provider.OperationRef{}, err
	}
	image, err := h.image(ctx, p.Spec.Image, serverType.Architecture)
	if err != nil {
		return provider.OperationRef{}, err
	}
	opts := hcloud.ServerCreateOpts{
		Name: p.Name, ServerType: serverType, Image: image,
		UserData: p.Spec.UserData, Labels: mergeLabels(p.Spec.Labels, p.Labels),
	}
	loc := p.Spec.Location
	if loc == "" {
		loc = h.location
	}
	if loc != "" {
		opts.Location = &hcloud.Location{Name: loc}
	}

	release, err := h.pacer.Acquire(ctx)
	if err != nil {
		return provider.OperationRef{}, err
	}
	defer release()
	result, resp, err := h.client.Server.Create(ctx, opts)
	h.observeResp(resp)
	if err != nil {
		// The request was sent: outcome uncertain unless proven otherwise.
		return provider.OperationRef{}, h.mapErr(err, provider.EffectMaybe)
	}

	actions := []int64{}
	if result.Action != nil {
		actions = append(actions, result.Action.ID)
	}
	for _, a := range result.NextActions {
		actions = append(actions, a.ID)
	}
	data, _ := json.Marshal(opData{V: 1, ServerID: result.Server.ID, Actions: actions})
	return provider.OperationRef{
		ActionID: action.ActionID,
		Ref:      &provider.ExternalRef{ID: strconv.FormatInt(result.Server.ID, 10)},
		Data:     data,
	}, nil
}

func (h *Hetzner) applyDelete(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	if action.Ref == nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: h.instance, Message: "delete requires a ref"}
	}
	id, err := strconv.ParseInt(action.Ref.ID, 10, 64)
	if err != nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: h.instance, Message: "malformed server id " + action.Ref.ID}
	}
	release, err := h.pacer.Acquire(ctx)
	if err != nil {
		return provider.OperationRef{}, err
	}
	defer release()
	srv, resp, err := h.client.Server.GetByID(ctx, id)
	h.observeResp(resp)
	if err != nil {
		return provider.OperationRef{}, h.mapErr(err, provider.EffectNone)
	}
	if srv == nil {
		// Delete of already-deleted is success (docs/03 §6, FI-7).
		return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref}, nil
	}
	result, resp, err := h.client.Server.DeleteWithResult(ctx, srv)
	h.observeResp(resp)
	if err != nil {
		return provider.OperationRef{}, h.mapErr(err, provider.EffectMaybe)
	}
	var actions []int64
	if result.Action != nil {
		actions = append(actions, result.Action.ID)
	}
	data, _ := json.Marshal(opData{V: 1, ServerID: id, Delete: true, Actions: actions})
	return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref, Data: data}, nil
}

func (h *Hetzner) ObserveOperation(ctx context.Context, op provider.OperationRef) (provider.OperationStatus, error) {
	var data opData
	if len(op.Data) > 0 {
		_ = json.Unmarshal(op.Data, &data)
	}
	// Poll pending provider actions by ID — never lists (05 §10).
	for _, actionID := range data.Actions {
		release, err := h.pacer.Acquire(ctx)
		if err != nil {
			return provider.OperationStatus{}, err
		}
		act, resp, err := h.client.Action.GetByID(ctx, actionID)
		h.observeResp(resp)
		release()
		if err != nil {
			return provider.OperationStatus{}, h.mapErr(err, provider.EffectNone)
		}
		if act == nil {
			continue // old actions expire server-side; fall back to server status
		}
		switch act.Status {
		case hcloud.ActionStatusError:
			return provider.OperationStatus{State: provider.OpFailed, Ref: op.Ref, Failure: &provider.Error{
				Class: provider.ErrTerminal, Provider: h.instance,
				Code: act.ErrorCode, Message: act.ErrorMessage,
			}}, nil
		case hcloud.ActionStatusRunning:
			return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref, RetryAfter: 2 * time.Second}, nil
		}
	}

	// Actions done (or Data lost): the server's state decides.
	if op.Ref == nil {
		return provider.OperationStatus{State: provider.OpUnknown}, nil
	}
	obs, err := h.Get(ctx, *op.Ref)
	if err != nil {
		return provider.OperationStatus{}, err // incl. synthesized not_found — engine interprets by kind
	}
	if data.Delete {
		return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref, RetryAfter: 2 * time.Second}, nil
	}
	switch obs.Phase {
	case provider.PhaseRunning:
		return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref, Resource: &obs}, nil
	default:
		return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref, RetryAfter: 2 * time.Second}, nil
	}
}

// --- lookups ---

func (h *Hetzner) serverType(ctx context.Context, name string) (*hcloud.ServerType, error) {
	release, err := h.pacer.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	st, resp, err := h.client.ServerType.GetByName(ctx, name)
	h.observeResp(resp)
	if err != nil {
		return nil, h.mapErr(err, provider.EffectNone)
	}
	if st == nil {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: h.instance, Message: "unknown server type " + name}
	}
	return st, nil
}

// image resolves "id:<n>" | "name:<os>" | "snapshot:<label-selector>"
// (snapshots have no names — label selection, newest wins, is the
// ecosystem convention).
func (h *Hetzner) image(ctx context.Context, spec string, arch hcloud.Architecture) (*hcloud.Image, error) {
	kind, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: h.instance, Message: `image must be "id:<n>", "name:<os>" or "snapshot:<label-selector>"`}
	}
	release, err := h.pacer.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	switch kind {
	case "id":
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: h.instance, Message: "malformed image id " + rest}
		}
		img, resp, err := h.client.Image.GetByID(ctx, id)
		h.observeResp(resp)
		if err != nil {
			return nil, h.mapErr(err, provider.EffectNone)
		}
		if img == nil {
			return nil, h.imageNotFound(spec)
		}
		return img, nil
	case "name":
		imgs, err := h.client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{
			Name: rest, Architecture: []hcloud.Architecture{arch},
		})
		if err != nil {
			return nil, h.mapErr(err, provider.EffectNone)
		}
		if len(imgs) == 0 {
			return nil, h.imageNotFound(spec)
		}
		return imgs[0], nil
	case "snapshot":
		imgs, err := h.client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{
			ListOpts: hcloud.ListOpts{LabelSelector: rest},
			Type:     []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		})
		if err != nil {
			return nil, h.mapErr(err, provider.EffectNone)
		}
		if len(imgs) == 0 {
			return nil, h.imageNotFound(spec)
		}
		sort.Slice(imgs, func(i, j int) bool { return imgs[i].Created.After(imgs[j].Created) })
		return imgs[0], nil
	default:
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: h.instance, Message: "unknown image form " + kind}
	}
}

func (h *Hetzner) imageNotFound(spec string) error {
	// A selector matching nothing is a config error: fail fast, not retry.
	return &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
		Provider: h.instance, Message: "no image matches " + spec}
}

// --- mapping ---

func (h *Hetzner) observe(srv *hcloud.Server) provider.ObservedResource {
	labels := srv.Labels
	var addrs []provider.Address
	if !srv.PublicNet.IPv4.IsUnspecified() && srv.PublicNet.IPv4.IP != nil {
		addrs = append(addrs, provider.Address{Network: "public-v4", Addr: srv.PublicNet.IPv4.IP.String()})
	}
	if srv.PublicNet.IPv6.IP != nil {
		addrs = append(addrs, provider.Address{Network: "public-v6", Addr: srv.PublicNet.IPv6.IP.String()})
	}
	for _, pn := range srv.PrivateNet {
		if pn.IP != nil {
			addrs = append(addrs, provider.Address{Network: "private", Addr: pn.IP.String()})
		}
	}
	cap := provider.Capacity{}
	if srv.ServerType != nil {
		cap[compute.DimCPU] = int64(srv.ServerType.Cores)
		cap[compute.DimMemoryMiB] = int64(srv.ServerType.Memory * 1024)
	}
	ext, _ := json.Marshal(srv) // full native object (invariant 6)
	return provider.ObservedResource{
		Ref:           provider.ExternalRef{ID: strconv.FormatInt(srv.ID, 10)},
		Kind:          compute.Kind,
		FleetplaneID:  labels[provider.LabelID],
		CreateOpID:    labels[provider.LabelOp],
		Owned:         labels[provider.LabelManaged] == "true" && labels[provider.LabelOwner] == h.ownerID,
		Phase:         mapStatus(srv.Status),
		ProviderState: string(srv.Status),
		Capacity:      cap,
		Addresses:     addrs,
		Labels:        labels,
		Extensions:    ext,
		ObservedAt:    time.Now(),
	}
}

func mapStatus(s hcloud.ServerStatus) provider.ObservedPhase {
	switch s {
	case hcloud.ServerStatusInitializing, hcloud.ServerStatusStarting, hcloud.ServerStatusMigrating, hcloud.ServerStatusRebuilding:
		return provider.PhasePending
	case hcloud.ServerStatusRunning:
		return provider.PhaseRunning
	case hcloud.ServerStatusOff, hcloud.ServerStatusStopping:
		return provider.PhaseStopped
	case hcloud.ServerStatusDeleting:
		return provider.PhaseDeleting
	default:
		return provider.PhaseUnknown
	}
}

func (h *Hetzner) observeResp(resp *hcloud.Response) {
	if resp == nil || resp.Meta.Ratelimit.Limit == 0 {
		return // no rate headers present
	}
	h.pacer.Observe(resp.Meta.Ratelimit.Remaining, resp.Meta.Ratelimit.Reset)
}

// mapErr folds hcloud errors into the typed model (03 §7). effectIfSent is
// the SideEffect when the request may have reached the API.
func (h *Hetzner) mapErr(err error, effectIfSent provider.SideEffect) error {
	var herr hcloud.Error
	if hcloudAs(err, &herr) {
		e := &provider.Error{Code: string(herr.Code), Message: herr.Message, Provider: h.instance, Cause: err}
		switch herr.Code {
		case hcloud.ErrorCodeNotFound:
			e.Class, e.SideEffect = provider.ErrNotFound, provider.EffectNone
		case hcloud.ErrorCodeRateLimitExceeded: // incl. legacy limit_reached alias
			e.Class, e.SideEffect = provider.ErrRateLimited, provider.EffectNone
			e.RetryAfter = h.pacer.On429(0)
		case hcloud.ErrorCodeConflict, hcloud.ErrorCodeLocked, hcloud.ErrorCodeUniquenessError:
			e.Class, e.SideEffect = provider.ErrConflict, effectIfSent
		case hcloud.ErrorCodeResourceLimitExceeded:
			e.Class, e.SideEffect = provider.ErrQuota, provider.EffectNone
		case hcloud.ErrorCodeInvalidInput, hcloud.ErrorCodeUnauthorized, hcloud.ErrorCodeForbidden:
			e.Class, e.SideEffect = provider.ErrInvalid, provider.EffectNone
		default:
			e.Class, e.SideEffect = provider.ErrRetryable, effectIfSent
		}
		return e
	}
	// Transport-level failure: retryable; side effect unknown if sent.
	return &provider.Error{Class: provider.ErrRetryable, SideEffect: effectIfSent,
		Provider: h.instance, Message: err.Error(), Cause: err}
}

func hcloudAs(err error, target *hcloud.Error) bool {
	for e := err; e != nil; {
		if he, ok := e.(hcloud.Error); ok {
			*target = he
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func (h *Hetzner) notFound(id string) *provider.Error {
	return &provider.Error{Class: provider.ErrNotFound, SideEffect: provider.EffectNone,
		Provider: h.instance, Message: "server " + id + " not found"}
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
