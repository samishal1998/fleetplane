// Package digitalocean drives compute.machine on DigitalOcean droplets via
// godo — the Phase-8 portability proof (doc 09): a second real cloud with a
// DIFFERENT identity model (flat tags, not labels) behind the unchanged SDK.
// godo performs no retries by default; the engine stays the only retry
// authority (ADR-014).
package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/digitalocean/godo"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
	"github.com/samishal1998/fleetplane/providers/pacing"
)

const Driver = "digitalocean"

func init() {
	provider.Register(Driver, func(ctx context.Context, cfg provider.InstanceConfig) (provider.Provider, error) {
		return New(ctx, cfg)
	})
}

// Settings is the driver config block (03 §4).
type Settings struct {
	Token    string  `json:"token"` // secret:// reference (07 §4)
	Region   string  `json:"region,omitempty"`
	Endpoint string  `json:"endpoint,omitempty"` // test override
	RPS      float64 `json:"rps,omitempty"`
	Burst    int     `json:"burst,omitempty"`
	MaxConc  int     `json:"maxConcurrent,omitempty"`
}

type DigitalOcean struct {
	client   *godo.Client
	instance string
	ownerID  string
	region   string
	pacer    *pacing.Pacer
}

type tokenTransport struct{ token string }

func (t tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone)
}

func New(ctx context.Context, cfg provider.InstanceConfig) (*DigitalOcean, error) {
	var s Settings
	if len(cfg.Settings) > 0 && string(cfg.Settings) != "null" {
		if err := json.Unmarshal(cfg.Settings, &s); err != nil {
			return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: cfg.Instance, Message: "digitalocean settings: " + err.Error()}
		}
	}
	if s.Token == "" {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "digitalocean: token is required (secret:// reference)"}
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
	opts := []godo.ClientOpt{godo.SetUserAgent("fleetplane/v1alpha1")}
	if s.Endpoint != "" {
		opts = append(opts, godo.SetBaseURL(s.Endpoint))
	}
	client, err := godo.New(&http.Client{Transport: tokenTransport{token: token}, Timeout: 60 * time.Second}, opts...)
	if err != nil {
		return nil, err
	}
	return &DigitalOcean{
		client:   client,
		instance: cfg.Instance,
		ownerID:  cfg.OwnerID,
		region:   s.Region,
		pacer:    pacing.New(s.RPS, s.Burst, s.MaxConc, nil),
	}, nil
}

// --- provider.Provider ---

// Billing implements provider.BillingAware: droplets bill per started hour
// (capped monthly — the hourly increment is the conservative model for
// window scheduling). Volumes are not modeled (fine-grained default).
func (d *DigitalOcean) Billing(kind provider.ResourceKind) provider.BillingPolicy {
	if kind == compute.Kind {
		return provider.BillingPolicy{BillingIncrement: time.Hour, TerminationBuffer: 5 * time.Minute}
	}
	return provider.BillingPolicy{}
}

func (d *DigitalOcean) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Driver: Driver, Instance: d.instance, Version: "godo",
		Kinds:                  []provider.ResourceKind{compute.Kind},
		SupportsLabelDiscovery: true,
	}
}

func (d *DigitalOcean) Capabilities(context.Context) ([]provider.CapabilityID, error) {
	return []provider.CapabilityID{"compute.machine.create"}, nil
}

func (d *DigitalOcean) ResourceDriver(kind provider.ResourceKind) (provider.ResourceDriver, bool) {
	if kind != compute.Kind {
		return nil, false
	}
	return d, true
}

func (d *DigitalOcean) Health(ctx context.Context) error {
	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	_, resp, err := d.client.Regions.List(ctx, &godo.ListOptions{PerPage: 1})
	d.observeResp(resp)
	if err != nil {
		return d.mapErr(err, provider.EffectNone)
	}
	return nil
}

func (d *DigitalOcean) Close() error { return nil }

// --- provider.ResourceDriver ---

func (d *DigitalOcean) Kind() provider.ResourceKind { return compute.Kind }

// Discover: DO lists by ONE tag at a time, so the driver picks the most
// selective tag as the server-side filter and applies the remaining label
// equalities client-side — the Discover contract is unchanged.
func (d *DigitalOcean) Discover(ctx context.Context, req provider.DiscoverRequest) ([]provider.ObservedResource, error) {
	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	want := map[string]string{}
	if req.Scope != provider.ScopeAll {
		want[provider.LabelManaged] = "true"
		want[provider.LabelOwner] = d.ownerID
	}
	for k, v := range req.Selector {
		want[k] = v
	}
	primary := ""
	for _, key := range []string{provider.LabelOp, provider.LabelID, provider.LabelOwner} {
		if v, ok := want[key]; ok {
			if tag, ok := encodeTag(key, v); ok {
				primary = tag
				break
			}
		}
	}

	var droplets []godo.Droplet
	page := &godo.ListOptions{PerPage: 100}
	for {
		var batch []godo.Droplet
		var resp *godo.Response
		if primary != "" {
			batch, resp, err = d.client.Droplets.ListByTag(ctx, primary, page)
		} else {
			batch, resp, err = d.client.Droplets.List(ctx, page)
		}
		d.observeResp(resp)
		if err != nil {
			return nil, d.mapErr(err, provider.EffectNone)
		}
		droplets = append(droplets, batch...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		page.Page++
		if page.Page == 1 {
			page.Page = 2
		}
	}

	var out []provider.ObservedResource
	for i := range droplets {
		obs := d.observe(&droplets[i])
		if matches(obs.Labels, want) {
			out = append(out, obs)
		}
	}
	return out, nil
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (d *DigitalOcean) Get(ctx context.Context, ref provider.ExternalRef) (provider.ObservedResource, error) {
	id, err := strconv.Atoi(ref.ID)
	if err != nil {
		return provider.ObservedResource{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance, Message: "malformed droplet id " + ref.ID}
	}
	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return provider.ObservedResource{}, err
	}
	defer release()
	droplet, resp, err := d.client.Droplets.Get(ctx, id)
	d.observeResp(resp)
	if err != nil {
		return provider.ObservedResource{}, d.mapErr(err, provider.EffectNone)
	}
	return d.observe(droplet), nil
}

func (d *DigitalOcean) Plan(_ context.Context, req provider.PlanRequest) (provider.Plan, error) {
	switch {
	case req.Desired != nil && req.Observed == nil:
		spec, err := compute.ParseSpec(req.Desired.Spec)
		if err != nil {
			return provider.Plan{}, &provider.Error{Class: provider.ErrInvalid,
				SideEffect: provider.EffectNone, Provider: d.instance, Message: err.Error()}
		}
		params, err := json.Marshal(createParams{Name: req.Desired.Name, Spec: *spec, Labels: req.Desired.Labels})
		if err != nil {
			return provider.Plan{}, err
		}
		return provider.Plan{
			Actions: []provider.Action{{Kind: "create", ResourceID: req.ResourceID, Params: params}},
			Summary: []string{fmt.Sprintf("create droplet %q (%s, %s)", req.Desired.Name, spec.ServerType, spec.Image)},
		}, nil
	case req.Desired == nil && req.Observed != nil:
		return provider.Plan{
			Actions: []provider.Action{{Kind: "delete", ResourceID: req.ResourceID, Ref: &req.Observed.Ref, Destructive: true}},
			Summary: []string{"delete droplet " + req.Observed.Ref.ID},
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

type opData struct {
	V         int  `json:"v"`
	DropletID int  `json:"dropletId"`
	Delete    bool `json:"delete,omitempty"`
}

func (d *DigitalOcean) Apply(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	switch action.Kind {
	case "create":
		return d.applyCreate(ctx, action)
	case "delete":
		return d.applyDelete(ctx, action)
	default:
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance, Message: "unknown action kind " + action.Kind}
	}
}

func (d *DigitalOcean) applyCreate(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	var p createParams
	if err := json.Unmarshal(action.Params, &p); err != nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance, Message: "malformed create params: " + err.Error()}
	}

	// Op-label dedup (the invariant-7 anchor) via the op tag.
	if existing, err := d.Discover(ctx, provider.DiscoverRequest{
		Scope: provider.ScopeOwned, Selector: map[string]string{provider.LabelOp: action.ActionID},
	}); err == nil && len(existing) > 0 {
		return provider.OperationRef{ActionID: action.ActionID, Ref: &existing[0].Ref}, nil
	}

	image, err := d.image(ctx, p.Spec.Image)
	if err != nil {
		return provider.OperationRef{}, err
	}
	region := p.Spec.Location
	if region == "" {
		region = d.region
	}
	tags := encodeTags(p.Labels)
	for k, v := range p.Spec.Labels {
		if tag, ok := encodeTag(k, v); ok {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)

	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return provider.OperationRef{}, err
	}
	defer release()
	droplet, resp, err := d.client.Droplets.Create(ctx, &godo.DropletCreateRequest{
		Name: p.Name, Region: region, Size: p.Spec.ServerType,
		Image: image, UserData: p.Spec.UserData, Tags: tags,
	})
	d.observeResp(resp)
	if err != nil {
		return provider.OperationRef{}, d.mapErr(err, provider.EffectMaybe)
	}
	data, _ := json.Marshal(opData{V: 1, DropletID: droplet.ID})
	return provider.OperationRef{
		ActionID: action.ActionID,
		Ref:      &provider.ExternalRef{ID: strconv.Itoa(droplet.ID)},
		Data:     data,
	}, nil
}

func (d *DigitalOcean) applyDelete(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	if action.Ref == nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance, Message: "delete requires a ref"}
	}
	id, err := strconv.Atoi(action.Ref.ID)
	if err != nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance, Message: "malformed droplet id " + action.Ref.ID}
	}
	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return provider.OperationRef{}, err
	}
	defer release()
	resp, err := d.client.Droplets.Delete(ctx, id)
	d.observeResp(resp)
	if err != nil {
		mapped := d.mapErr(err, provider.EffectMaybe)
		if provider.IsClass(mapped, provider.ErrNotFound) {
			// Delete of already-deleted is success (docs/03 §6, FI-7).
			return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref}, nil
		}
		return provider.OperationRef{}, mapped
	}
	data, _ := json.Marshal(opData{V: 1, DropletID: id, Delete: true})
	return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref, Data: data}, nil
}

func (d *DigitalOcean) ObserveOperation(ctx context.Context, op provider.OperationRef) (provider.OperationStatus, error) {
	if op.Ref == nil {
		return provider.OperationStatus{State: provider.OpUnknown}, nil
	}
	var data opData
	if len(op.Data) > 0 {
		_ = json.Unmarshal(op.Data, &data)
	}
	obs, err := d.Get(ctx, *op.Ref)
	if err != nil {
		return provider.OperationStatus{}, err // not_found: the engine interprets by op kind
	}
	if data.Delete {
		return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref, RetryAfter: 2 * time.Second}, nil
	}
	switch obs.Phase {
	case provider.PhaseRunning:
		return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref, Resource: &obs}, nil
	default: // "new" droplets are pending
		return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref, RetryAfter: 2 * time.Second}, nil
	}
}

// image resolves "id:<n>" | "slug:<distro-slug>" | "snapshot:<name>"
// (DO snapshots HAVE names — a nice contrast with Hetzner; newest wins on
// duplicates).
func (d *DigitalOcean) image(ctx context.Context, spec string) (godo.DropletCreateImage, error) {
	kind, rest, ok := cut(spec)
	if !ok {
		return godo.DropletCreateImage{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance,
			Message: `image must be "id:<n>", "slug:<slug>" or "snapshot:<name>"`}
	}
	switch kind {
	case "id":
		id, err := strconv.Atoi(rest)
		if err != nil {
			return godo.DropletCreateImage{}, &provider.Error{Class: provider.ErrInvalid,
				SideEffect: provider.EffectNone, Provider: d.instance, Message: "malformed image id " + rest}
		}
		return godo.DropletCreateImage{ID: id}, nil
	case "slug":
		return godo.DropletCreateImage{Slug: rest}, nil
	case "snapshot":
		release, err := d.pacer.Acquire(ctx)
		if err != nil {
			return godo.DropletCreateImage{}, err
		}
		defer release()
		var best *godo.Image
		page := &godo.ListOptions{PerPage: 100}
		for {
			images, resp, err := d.client.Images.ListUser(ctx, page)
			d.observeResp(resp)
			if err != nil {
				return godo.DropletCreateImage{}, d.mapErr(err, provider.EffectNone)
			}
			for i := range images {
				img := &images[i]
				if img.Name != rest {
					continue
				}
				if best == nil || img.Created > best.Created {
					best = img
				}
			}
			if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
				break
			}
			page.Page++
			if page.Page == 1 {
				page.Page = 2
			}
		}
		if best == nil {
			return godo.DropletCreateImage{}, &provider.Error{Class: provider.ErrInvalid,
				SideEffect: provider.EffectNone, Provider: d.instance,
				Message: "no snapshot named " + rest + " (fail fast, not retry)"}
		}
		return godo.DropletCreateImage{ID: best.ID}, nil
	default:
		return godo.DropletCreateImage{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance, Message: "unknown image form " + kind}
	}
}

func cut(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], s[i+1:], i > 0 && i < len(s)-1
		}
	}
	return "", "", false
}

// --- mapping ---

func (d *DigitalOcean) observe(dr *godo.Droplet) provider.ObservedResource {
	labels := decodeTags(dr.Tags)
	var addrs []provider.Address
	if dr.Networks != nil {
		for _, n := range dr.Networks.V4 {
			net := "private"
			if n.Type == "public" {
				net = "public-v4"
			}
			addrs = append(addrs, provider.Address{Network: net, Addr: n.IPAddress})
		}
		for _, n := range dr.Networks.V6 {
			if n.Type == "public" {
				addrs = append(addrs, provider.Address{Network: "public-v6", Addr: n.IPAddress})
			}
		}
	}
	var ph provider.ObservedPhase
	switch dr.Status {
	case "new":
		ph = provider.PhasePending
	case "active":
		ph = provider.PhaseRunning
	case "off":
		ph = provider.PhaseStopped
	case "archive":
		ph = provider.PhaseGone
	default:
		ph = provider.PhaseUnknown
	}
	ext, _ := json.Marshal(dr) // full native object (invariant 6)
	return provider.ObservedResource{
		Ref:           provider.ExternalRef{ID: strconv.Itoa(dr.ID)},
		Kind:          compute.Kind,
		FleetplaneID:  labels[provider.LabelID],
		CreateOpID:    labels[provider.LabelOp],
		Owned:         labels[provider.LabelManaged] == "true" && labels[provider.LabelOwner] == d.ownerID,
		Phase:         ph,
		ProviderState: dr.Status,
		Capacity: provider.Capacity{
			compute.DimCPU:       int64(dr.Vcpus),
			compute.DimMemoryMiB: int64(dr.Memory), // godo reports MB ≈ MiB
		},
		Addresses:  addrs,
		Labels:     labels,
		Extensions: ext,
		ObservedAt: time.Now(),
	}
}

func (d *DigitalOcean) observeResp(resp *godo.Response) {
	if resp == nil || resp.Limit == 0 {
		return
	}
	d.pacer.Observe(resp.Remaining, resp.Reset.Time)
}

func (d *DigitalOcean) mapErr(err error, effectIfSent provider.SideEffect) error {
	var gerr *godo.ErrorResponse
	if ok := asGodoErr(err, &gerr); ok && gerr.Response != nil {
		e := &provider.Error{Provider: d.instance, Message: gerr.Message, RequestID: gerr.RequestID, Cause: err}
		switch code := gerr.Response.StatusCode; {
		case code == http.StatusNotFound:
			e.Class, e.SideEffect = provider.ErrNotFound, provider.EffectNone
		case code == http.StatusTooManyRequests:
			e.Class, e.SideEffect = provider.ErrRateLimited, provider.EffectNone
			e.RetryAfter = d.pacer.On429(0)
		case code == http.StatusConflict || code == http.StatusLocked:
			e.Class, e.SideEffect = provider.ErrConflict, effectIfSent
		case code == http.StatusUnprocessableEntity || code == http.StatusBadRequest ||
			code == http.StatusUnauthorized || code == http.StatusForbidden:
			e.Class, e.SideEffect = provider.ErrInvalid, provider.EffectNone
		case code >= 500:
			e.Class, e.SideEffect = provider.ErrRetryable, effectIfSent
		default:
			e.Class, e.SideEffect = provider.ErrRetryable, effectIfSent
		}
		return e
	}
	return &provider.Error{Class: provider.ErrRetryable, SideEffect: effectIfSent,
		Provider: d.instance, Message: err.Error(), Cause: err}
}

func asGodoErr(err error, target **godo.ErrorResponse) bool {
	for e := err; e != nil; {
		if ge, ok := e.(*godo.ErrorResponse); ok {
			*target = ge
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
