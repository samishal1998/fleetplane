// Package gcp drives compute.machine on Google Compute Engine via the
// google.golang.org/api/compute/v1 REST client (docs/09: portability).
//
// Retry discipline (ADR-014): the generated JSON API calls go through
// gensupport.SendRequest, which performs NO automatic retries (the retrying
// path is media-upload only, unused here) — the operation engine stays the
// single retry authority.
//
// Credentials: settings.credentialsJson is a secret:// reference to a
// service-account JSON key, resolved at construction time via cfg.Secrets.
// When it is omitted entirely the driver falls back to the SDK's ambient
// Application Default Credentials chain (GOOGLE_APPLICATION_CREDENTIALS,
// gcloud user creds, metadata server). When settings.endpoint is set (test
// override) the service is built with option.WithoutAuthentication.
//
// Scope: discovery is project-wide (instances.aggregatedList across all
// zones — creates honor spec.location, so owned instances may live outside
// the default zone); create-dedup lists the deterministic create zone. The
// zone travels in ExternalRef.Extra["zone"] for Get/delete/observe.
package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth/credentials"
	gce "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
	"github.com/samishal1998/fleetplane/providers/pacing"
)

const Driver = "gcp"

func init() {
	provider.Register(Driver, func(ctx context.Context, cfg provider.InstanceConfig) (provider.Provider, error) {
		return New(ctx, cfg)
	})
}

// Settings is the driver config block (03 §4).
type Settings struct {
	Project         string  `json:"project"`                   // required
	Zone            string  `json:"zone"`                      // required; default zone for instances
	CredentialsJSON string  `json:"credentialsJson,omitempty"` // secret:// ref to a service-account key; omitted => ADC
	Endpoint        string  `json:"endpoint,omitempty"`        // test override
	Network         string  `json:"network,omitempty"`
	Subnetwork      string  `json:"subnetwork,omitempty"`
	RPS             float64 `json:"rps,omitempty"`
	Burst           int     `json:"burst,omitempty"`
	MaxConc         int     `json:"maxConcurrent,omitempty"`
}

type GCP struct {
	svc        *gce.Service
	instance   string
	ownerID    string
	project    string
	zone       string
	network    string
	subnetwork string
	pacer      *pacing.Pacer

	mtMu    sync.Mutex
	mtCache map[string]provider.Capacity // "<zone>/<machineType>" -> capacity
}

func New(ctx context.Context, cfg provider.InstanceConfig) (*GCP, error) {
	var s Settings
	if len(cfg.Settings) > 0 && string(cfg.Settings) != "null" {
		if err := json.Unmarshal(cfg.Settings, &s); err != nil {
			return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: cfg.Instance, Message: "gcp settings: " + err.Error()}
		}
	}
	if s.Project == "" {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "gcp: project is required"}
	}
	if s.Zone == "" {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "gcp: zone is required"}
	}
	if s.CredentialsJSON != "" && !secretref.IsRef(s.CredentialsJSON) {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "gcp: credentialsJson must be a secret:// reference (07 §4)"}
	}

	opts := []option.ClientOption{option.WithUserAgent("fleetplane/v1alpha1")}
	switch {
	case s.Endpoint != "":
		opts = append(opts, option.WithEndpoint(s.Endpoint), option.WithoutAuthentication())
	case s.CredentialsJSON != "":
		sec, err := cfg.Secrets.Resolve(ctx, s.CredentialsJSON)
		if err != nil {
			return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: cfg.Instance, Message: err.Error()}
		}
		// option.WithCredentialsJSON is deprecated; detect explicitly via
		// the auth library so the key JSON never rides an option value.
		creds, err := credentials.DetectDefault(&credentials.DetectOptions{
			CredentialsJSON: sec.Reveal(),
			Scopes:          []string{gce.CloudPlatformScope},
		})
		if err != nil {
			return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: cfg.Instance, Message: "gcp: credentials: " + err.Error(), Cause: err}
		}
		opts = append(opts, option.WithAuthCredentials(creds))
	default:
		// Ambient Application Default Credentials (see package doc).
	}
	svc, err := gce.NewService(ctx, opts...)
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "gcp: constructing compute service: " + err.Error(), Cause: err}
	}
	return &GCP{
		svc:        svc,
		instance:   cfg.Instance,
		ownerID:    cfg.OwnerID,
		project:    s.Project,
		zone:       s.Zone,
		network:    s.Network,
		subnetwork: s.Subnetwork,
		pacer:      pacing.New(s.RPS, s.Burst, s.MaxConc, nil),
		mtCache:    map[string]provider.Capacity{},
	}, nil
}

// --- provider.Provider ---

// Billing implements provider.BillingAware: GCE bills per second with a
// 60-second minimum (docs/11). Kinds this driver does not serve get the
// zero (fine-grained) policy — the capability contract.
func (g *GCP) Billing(kind provider.ResourceKind) provider.BillingPolicy {
	if kind == compute.Kind {
		return provider.BillingPolicy{MinimumDuration: time.Minute}
	}
	return provider.BillingPolicy{}
}

func (g *GCP) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Driver: Driver, Instance: g.instance, Version: "compute/v1",
		Kinds:                  []provider.ResourceKind{compute.Kind},
		SupportsLabelDiscovery: true,
	}
}

func (g *GCP) Capabilities(context.Context) ([]provider.CapabilityID, error) {
	return []provider.CapabilityID{"compute.machine.create"}, nil
}

func (g *GCP) ResourceDriver(kind provider.ResourceKind) (provider.ResourceDriver, bool) {
	if kind != compute.Kind {
		return nil, false
	}
	return g, true
}

func (g *GCP) Health(ctx context.Context) error {
	release, err := g.pacer.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	// Cheap authenticated read.
	if _, err := g.svc.Zones.Get(g.project, g.zone).Context(ctx).Do(); err != nil {
		return g.mapErr(err, provider.EffectNone)
	}
	return nil
}

func (g *GCP) Close() error { return nil }

// --- provider.ResourceDriver ---

func (g *GCP) Kind() provider.ResourceKind { return compute.Kind }

// Discover lists instances project-wide (every zone: creates honor
// spec.location, so an owned instance outside the default zone must still be
// swept). The identity/selector equalities are pushed down as a server-side
// labels filter (encoded through the codec) and re-verified client-side
// against the DECODED labels, so the Discover contract holds regardless of
// server-side filter fidelity.
func (g *GCP) Discover(ctx context.Context, req provider.DiscoverRequest) ([]provider.ObservedResource, error) {
	want := map[string]string{}
	if req.Scope != provider.ScopeAll {
		want[provider.LabelManaged] = "true"
		want[provider.LabelOwner] = g.ownerID
	}
	for k, v := range req.Selector {
		want[k] = v
	}
	var parts []string
	for k, v := range want {
		parts = append(parts, fmt.Sprintf("labels.%s = %q", encodeKey(k), encodeValue(v)))
	}
	sort.Strings(parts)

	instances, err := g.listAllInstances(ctx, strings.Join(parts, " AND "))
	if err != nil {
		return nil, err
	}
	var out []provider.ObservedResource
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		obs := g.observe(ctx, inst)
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

// listAllInstances is a fully paginated project-wide
// instances.aggregatedList. Partial success is NOT requested: an ownership
// sweep that silently skips unreachable zones could declare live resources
// gone, so zone outages surface as errors instead.
func (g *GCP) listAllInstances(ctx context.Context, filter string) ([]*gce.Instance, error) {
	var out []*gce.Instance
	pageToken := ""
	for {
		release, err := g.pacer.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		call := g.svc.Instances.AggregatedList(g.project).Context(ctx)
		if filter != "" {
			call = call.Filter(filter)
		}
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		list, err := call.Do()
		release()
		if err != nil {
			return nil, g.mapErr(err, provider.EffectNone)
		}
		for _, scoped := range list.Items {
			out = append(out, scoped.Instances...)
		}
		if list.NextPageToken == "" {
			return out, nil
		}
		pageToken = list.NextPageToken
	}
}

// listInstances is a fully paginated instances.list in one zone.
func (g *GCP) listInstances(ctx context.Context, zone, filter string) ([]*gce.Instance, error) {
	var out []*gce.Instance
	pageToken := ""
	for {
		release, err := g.pacer.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		call := g.svc.Instances.List(g.project, zone).Context(ctx)
		if filter != "" {
			call = call.Filter(filter)
		}
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		list, err := call.Do()
		release()
		if err != nil {
			return nil, g.mapErr(err, provider.EffectNone)
		}
		out = append(out, list.Items...)
		if list.NextPageToken == "" {
			return out, nil
		}
		pageToken = list.NextPageToken
	}
}

func (g *GCP) Get(ctx context.Context, ref provider.ExternalRef) (provider.ObservedResource, error) {
	if ref.ID == "" {
		return provider.ObservedResource{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: g.instance, Message: "empty instance name"}
	}
	release, err := g.pacer.Acquire(ctx)
	if err != nil {
		return provider.ObservedResource{}, err
	}
	defer release()
	inst, err := g.svc.Instances.Get(g.project, g.zoneOf(ref), ref.ID).Context(ctx).Do()
	if err != nil {
		return provider.ObservedResource{}, g.mapErr(err, provider.EffectNone)
	}
	return g.observe(ctx, inst), nil
}

func (g *GCP) Plan(_ context.Context, req provider.PlanRequest) (provider.Plan, error) {
	switch {
	case req.Desired != nil && req.Observed == nil:
		spec, err := compute.ParseSpec(req.Desired.Spec)
		if err != nil {
			return provider.Plan{}, &provider.Error{Class: provider.ErrInvalid,
				SideEffect: provider.EffectNone, Provider: g.instance, Message: err.Error()}
		}
		params, err := json.Marshal(createParams{Name: req.Desired.Name, Spec: *spec, Labels: req.Desired.Labels})
		if err != nil {
			return provider.Plan{}, err
		}
		return provider.Plan{
			Actions: []provider.Action{{Kind: "create", ResourceID: req.ResourceID, Params: params}},
			Summary: []string{fmt.Sprintf("create gce instance %q (%s, %s)", req.Desired.Name, spec.ServerType, spec.Image)},
		}, nil
	case req.Desired == nil && req.Observed != nil:
		return provider.Plan{
			Actions: []provider.Action{{Kind: "delete", ResourceID: req.ResourceID, Ref: &req.Observed.Ref, Destructive: true}},
			Summary: []string{"delete gce instance " + req.Observed.Ref.ID},
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

// opData is OperationRef.Data (self-versioned; the driver tolerates nil by
// degrading to instance-status observation).
type opData struct {
	V        int    `json:"v"`
	Op       string `json:"op"` // zonal operation name
	Zone     string `json:"zone"`
	Instance string `json:"instance"`
	Delete   bool   `json:"delete,omitempty"`
}

func (g *GCP) Apply(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	switch action.Kind {
	case "create":
		return g.applyCreate(ctx, action)
	case "delete":
		return g.applyDelete(ctx, action)
	default:
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: g.instance, Message: "unknown action kind " + action.Kind}
	}
}

func (g *GCP) applyCreate(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	var p createParams
	if err := json.Unmarshal(action.Params, &p); err != nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: g.instance, Message: "malformed create params: " + err.Error()}
	}
	zone := p.Spec.Location
	if zone == "" {
		zone = g.zone
	}

	// Op-label dedup (invariant 7): a replayed create returns the instance
	// this SAME operation already produced instead of creating a second one.
	if ref := g.findByOp(ctx, zone, action.ActionID); ref != nil {
		return provider.OperationRef{ActionID: action.ActionID, Ref: ref}, nil
	}

	sourceImage, err := g.image(ctx, p.Spec.Image)
	if err != nil {
		return provider.OperationRef{}, err
	}

	network := g.network
	if network == "" {
		network = "global/networks/default"
	}
	ni := &gce.NetworkInterface{
		Network: network,
		// Ephemeral external IP.
		AccessConfigs: []*gce.AccessConfig{{Type: "ONE_TO_ONE_NAT", Name: "External NAT"}},
	}
	if g.subnetwork != "" {
		ni.Subnetwork = g.subnetwork
	}
	inst := &gce.Instance{
		Name:        p.Name,
		MachineType: fmt.Sprintf("zones/%s/machineTypes/%s", zone, p.Spec.ServerType),
		Labels:      encodeLabels(mergeLabels(p.Spec.Labels, p.Labels)),
		Disks: []*gce.AttachedDisk{{
			Boot: true, AutoDelete: true,
			InitializeParams: &gce.AttachedDiskInitializeParams{SourceImage: sourceImage},
		}},
		NetworkInterfaces: []*gce.NetworkInterface{ni},
	}
	if p.Spec.UserData != "" {
		// cloud-init convention: metadata key "user-data".
		ud := p.Spec.UserData
		inst.Metadata = &gce.Metadata{Items: []*gce.MetadataItems{{Key: "user-data", Value: &ud}}}
	}

	release, err := g.pacer.Acquire(ctx)
	if err != nil {
		return provider.OperationRef{}, g.preflight(err)
	}
	defer release()
	op, err := g.svc.Instances.Insert(g.project, zone, inst).Context(ctx).Do()
	if err != nil {
		// The request was sent: outcome uncertain unless proven otherwise.
		return provider.OperationRef{}, g.mapErr(err, provider.EffectMaybe)
	}
	data, _ := json.Marshal(opData{V: 1, Op: op.Name, Zone: zone, Instance: p.Name})
	return provider.OperationRef{
		ActionID: action.ActionID,
		Ref:      &provider.ExternalRef{ID: p.Name, Extra: map[string]string{"zone": zone}},
		Data:     data,
	}, nil
}

// findByOp searches the zone for a live instance carrying this operation's
// identity. Lookup failures fall through to create (best-effort pre-flight;
// a true duplicate insert then fails on the name conflict).
func (g *GCP) findByOp(ctx context.Context, zone, actionID string) *provider.ExternalRef {
	filter := fmt.Sprintf("labels.%s = %q", shortNames[provider.LabelOp], encodeValue(actionID))
	instances, err := g.listInstances(ctx, zone, filter)
	if err != nil {
		return nil
	}
	for _, inst := range instances {
		if inst == nil || inst.Status == "TERMINATED" || inst.Status == "DEPROVISIONING" {
			continue // may be mid-deletion: never reuse (a still-live name
			// then fails the insert with a 409, not a second instance)
		}
		if decodeLabels(inst.Labels)[provider.LabelOp] != actionID {
			continue
		}
		z := lastSegment(inst.Zone)
		if z == "" {
			z = zone
		}
		return &provider.ExternalRef{ID: inst.Name, Extra: map[string]string{"zone": z}}
	}
	return nil
}

func (g *GCP) applyDelete(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	if action.Ref == nil || action.Ref.ID == "" {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: g.instance, Message: "delete requires a ref"}
	}
	zone := g.zoneOf(*action.Ref)
	release, err := g.pacer.Acquire(ctx)
	if err != nil {
		return provider.OperationRef{}, g.preflight(err)
	}
	defer release()
	op, err := g.svc.Instances.Delete(g.project, zone, action.Ref.ID).Context(ctx).Do()
	if err != nil {
		mapped := g.mapErr(err, provider.EffectMaybe)
		if provider.IsClass(mapped, provider.ErrNotFound) {
			// Delete of already-deleted is success (docs/03 §6, FI-7).
			return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref}, nil
		}
		return provider.OperationRef{}, mapped
	}
	data, _ := json.Marshal(opData{V: 1, Op: op.Name, Zone: zone, Instance: action.Ref.ID, Delete: true})
	return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref, Data: data}, nil
}

func (g *GCP) ObserveOperation(ctx context.Context, op provider.OperationRef) (provider.OperationStatus, error) {
	var data opData
	if len(op.Data) > 0 {
		_ = json.Unmarshal(op.Data, &data)
	}

	// Poll the zonal operation by reference — never by listing (05 §10).
	if data.Op != "" {
		zone := data.Zone
		if zone == "" {
			zone = g.zone
		}
		release, err := g.pacer.Acquire(ctx)
		if err != nil {
			return provider.OperationStatus{}, err
		}
		zop, err := g.svc.ZoneOperations.Get(g.project, zone, data.Op).Context(ctx).Do()
		release()
		switch {
		case err != nil:
			mapped := g.mapErr(err, provider.EffectNone)
			if !provider.IsClass(mapped, provider.ErrNotFound) {
				return provider.OperationStatus{}, mapped
			}
			// Operations expire server-side: fall back to instance status.
		case zop.Status == "PENDING":
			return provider.OperationStatus{State: provider.OpPending, Ref: op.Ref, RetryAfter: 2 * time.Second}, nil
		case zop.Status == "RUNNING":
			return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref, RetryAfter: 2 * time.Second}, nil
		case zop.Status == "DONE":
			if zop.Error != nil && len(zop.Error.Errors) > 0 {
				return provider.OperationStatus{State: provider.OpFailed, Ref: op.Ref, Failure: g.operationFailure(zop)}, nil
			}
			// DONE without error: the instance's state decides below.
		}
	}

	ref := op.Ref
	if ref == nil {
		if data.Instance == "" {
			return provider.OperationStatus{State: provider.OpUnknown}, nil
		}
		zone := data.Zone
		if zone == "" {
			zone = g.zone
		}
		ref = &provider.ExternalRef{ID: data.Instance, Extra: map[string]string{"zone": zone}}
	}
	obs, err := g.Get(ctx, *ref)
	if err != nil {
		// Incl. not_found — the engine interprets by op kind: a vanished
		// create routes to verifying, a vanished delete is completion.
		return provider.OperationStatus{}, err
	}
	if data.Delete {
		return provider.OperationStatus{State: provider.OpRunning, Ref: ref, RetryAfter: 2 * time.Second}, nil
	}
	switch obs.Phase {
	case provider.PhaseRunning:
		return provider.OperationStatus{State: provider.OpSucceeded, Ref: ref, Resource: &obs}, nil
	default:
		return provider.OperationStatus{State: provider.OpRunning, Ref: ref, RetryAfter: 2 * time.Second}, nil
	}
}

// operationFailure maps a DONE zonal operation's error into the typed model.
func (g *GCP) operationFailure(zop *gce.Operation) *provider.Error {
	oe := zop.Error.Errors[0]
	e := &provider.Error{SideEffect: provider.EffectMaybe, Provider: g.instance,
		Code: oe.Code, Message: oe.Message}
	switch {
	case strings.Contains(oe.Code, "QUOTA_EXCEEDED"):
		e.Class = provider.ErrQuota
	case strings.Contains(oe.Code, "NOT_FOUND"):
		e.Class = provider.ErrNotFound
	case strings.Contains(oe.Code, "RESOURCE_POOL_EXHAUSTED"),
		strings.Contains(oe.Code, "RATE_EXCEEDED"):
		e.Class = provider.ErrRetryable
	default:
		e.Class = provider.ErrTerminal
	}
	return e
}

// --- image resolution ---

// image resolves the spec's image selector to a sourceImage URL:
// "id:<self-link-or-name>" | "family:<[project/]family>" |
// "name:<[project/]name>" | "snapshot:<k=v>" (labeled image in the project,
// newest creationTimestamp wins).
func (g *GCP) image(ctx context.Context, spec string) (string, error) {
	form, rest, ok := strings.Cut(spec, ":")
	if !ok || form == "" || rest == "" {
		return "", g.invalidImage(`image must be "id:<image>", "family:<family>", "name:<name>" or "snapshot:<k=v>"`)
	}
	switch form {
	case "id":
		if strings.Contains(rest, "/") {
			return rest, nil // self-link or partial URL: pass through
		}
		return "projects/" + g.project + "/global/images/" + rest, nil
	case "family":
		proj, family := splitProject(rest, g.project)
		release, err := g.pacer.Acquire(ctx)
		if err != nil {
			return "", g.preflight(err)
		}
		img, err := g.svc.Images.GetFromFamily(proj, family).Context(ctx).Do()
		release()
		if err != nil {
			return "", g.imageLookupErr(err, spec)
		}
		return imageURL(img, proj), nil
	case "name":
		proj, name := splitProject(rest, g.project)
		release, err := g.pacer.Acquire(ctx)
		if err != nil {
			return "", g.preflight(err)
		}
		img, err := g.svc.Images.Get(proj, name).Context(ctx).Do()
		release()
		if err != nil {
			return "", g.imageLookupErr(err, spec)
		}
		return imageURL(img, proj), nil
	case "snapshot":
		k, v, ok := strings.Cut(rest, "=")
		if !ok || k == "" {
			return "", g.invalidImage("snapshot selector must be <label>=<value>")
		}
		encK, encV := encodeKey(k), encodeValue(v)
		var best *gce.Image
		var bestAt time.Time
		pageToken := ""
		for {
			release, err := g.pacer.Acquire(ctx)
			if err != nil {
				return "", g.preflight(err)
			}
			call := g.svc.Images.List(g.project).Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			list, err := call.Do()
			release()
			if err != nil {
				return "", g.mapErr(err, provider.EffectNone)
			}
			for _, img := range list.Items {
				// Client-side filter by the ENCODED label; newest wins.
				if img == nil || img.Labels[encK] != encV {
					continue
				}
				at, terr := time.Parse(time.RFC3339, img.CreationTimestamp)
				if terr != nil {
					at = time.Time{}
				}
				if best == nil || at.After(bestAt) {
					best, bestAt = img, at
				}
			}
			if list.NextPageToken == "" {
				break
			}
			pageToken = list.NextPageToken
		}
		if best == nil {
			return "", g.invalidImage("no image matches " + spec + " (fail fast, not retry)")
		}
		return imageURL(best, g.project), nil
	default:
		return "", g.invalidImage("unknown image form " + form)
	}
}

func (g *GCP) invalidImage(msg string) error {
	return &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
		Provider: g.instance, Message: msg}
}

// imageLookupErr turns a lookup 404 into fail-fast invalid (a selector
// matching nothing is a config error, not a retry candidate).
func (g *GCP) imageLookupErr(err error, spec string) error {
	mapped := g.mapErr(err, provider.EffectNone)
	if provider.IsClass(mapped, provider.ErrNotFound) {
		return g.invalidImage("no image matches " + spec)
	}
	return mapped
}

func imageURL(img *gce.Image, proj string) string {
	if img.SelfLink != "" {
		return img.SelfLink
	}
	return "projects/" + proj + "/global/images/" + img.Name
}

// splitProject splits "<project>/<value>" selectors; a bare value resolves
// within the configured project.
func splitProject(rest, def string) (string, string) {
	if proj, val, ok := strings.Cut(rest, "/"); ok && proj != "" && val != "" && !strings.Contains(val, "/") {
		return proj, val
	}
	return def, rest
}

// --- mapping ---

func (g *GCP) observe(ctx context.Context, inst *gce.Instance) provider.ObservedResource {
	zone := lastSegment(inst.Zone)
	if zone == "" {
		zone = g.zone
	}
	labels := decodeLabels(inst.Labels)
	var addrs []provider.Address
	for _, ni := range inst.NetworkInterfaces {
		if ni == nil {
			continue
		}
		if ni.NetworkIP != "" {
			// "private" is the SDK's network name for internal addresses
			// (provider.Address contract; readiness probes match on it).
			addrs = append(addrs, provider.Address{Network: "private", Addr: ni.NetworkIP})
		}
		for _, ac := range ni.AccessConfigs {
			if ac != nil && ac.NatIP != "" {
				addrs = append(addrs, provider.Address{Network: "public-v4", Addr: ac.NatIP})
			}
		}
	}
	ext, _ := json.Marshal(inst) // full native object (invariant 6)
	return provider.ObservedResource{
		Ref:           provider.ExternalRef{ID: inst.Name, Extra: map[string]string{"zone": zone}},
		Kind:          compute.Kind,
		FleetplaneID:  labels[provider.LabelID],
		CreateOpID:    labels[provider.LabelOp],
		Owned:         labels[provider.LabelManaged] == "true" && labels[provider.LabelOwner] == g.ownerID,
		Phase:         mapStatus(inst.Status),
		ProviderState: inst.Status,
		Capacity:      g.capacity(ctx, zone, lastSegment(inst.MachineType)),
		Addresses:     addrs,
		Labels:        labels,
		Extensions:    ext,
		ObservedAt:    time.Now(),
	}
}

// mapStatus normalizes the GCE instance status (the full compute/v1 enum:
// DEPROVISIONING, PENDING, PENDING_STOP, PROVISIONING, REPAIRING, RUNNING,
// STAGING, STOPPED, STOPPING, SUSPENDED, SUSPENDING, TERMINATED).
// TERMINATED/STOPPED are GCP's "stopped" (the instance still exists); a
// deleted instance 404s instead. DEPROVISIONING is teardown of a stop OR a
// delete — stopped is the conservative mapping, the 404 decides deletion.
// REPAIRING is genuinely indeterminate: unknown.
func mapStatus(s string) provider.ObservedPhase {
	switch s {
	case "PENDING", "PROVISIONING", "STAGING":
		return provider.PhasePending
	case "RUNNING":
		return provider.PhaseRunning
	case "PENDING_STOP", "STOPPING", "SUSPENDING", "SUSPENDED",
		"STOPPED", "TERMINATED", "DEPROVISIONING":
		return provider.PhaseStopped
	default:
		return provider.PhaseUnknown
	}
}

// capacity resolves machine-type dimensions via machineTypes.get with a
// per-instance cache. Best-effort: a failed lookup leaves capacity untracked
// rather than failing the observation.
func (g *GCP) capacity(ctx context.Context, zone, machineType string) provider.Capacity {
	if machineType == "" {
		return nil
	}
	key := zone + "/" + machineType
	g.mtMu.Lock()
	if c, ok := g.mtCache[key]; ok {
		g.mtMu.Unlock()
		return c
	}
	g.mtMu.Unlock()

	release, err := g.pacer.Acquire(ctx)
	if err != nil {
		return nil
	}
	mt, err := g.svc.MachineTypes.Get(g.project, zone, machineType).Context(ctx).Do()
	release()
	if err != nil || mt == nil {
		return nil
	}
	c := provider.Capacity{compute.DimCPU: mt.GuestCpus, compute.DimMemoryMiB: mt.MemoryMb}
	g.mtMu.Lock()
	g.mtCache[key] = c
	g.mtMu.Unlock()
	return c
}

func (g *GCP) zoneOf(ref provider.ExternalRef) string {
	if z := ref.Extra["zone"]; z != "" {
		return z
	}
	return g.zone
}

func lastSegment(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
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

// preflight types an error raised strictly BEFORE a mutating request was
// sent (pacer/context failures on Apply paths): provably EffectNone, so the
// engine never runs uncertainty resolution for a request that never left.
func (g *GCP) preflight(err error) error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return err // already typed (pacer park is EffectNone by construction)
	}
	return &provider.Error{Class: provider.Classify(err), SideEffect: provider.EffectNone,
		Provider: g.instance, Message: err.Error(), Cause: err}
}

// mapErr folds googleapi errors into the typed model (03 §7). effectIfSent
// is the SideEffect when the request may have reached the API.
func (g *GCP) mapErr(err error, effectIfSent provider.SideEffect) error {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		e := &provider.Error{Provider: g.instance, Code: strconv.Itoa(gerr.Code),
			Message: gerr.Message, Cause: err}
		if e.Message == "" {
			e.Message = "googleapi error " + e.Code
		}
		reasons := map[string]bool{}
		for _, item := range gerr.Errors {
			reasons[item.Reason] = true
		}
		switch {
		case gerr.Code == http.StatusNotFound:
			e.Class, e.SideEffect = provider.ErrNotFound, provider.EffectNone
		case gerr.Code == http.StatusTooManyRequests,
			gerr.Code == http.StatusForbidden && (reasons["rateLimitExceeded"] || reasons["userRateLimitExceeded"]):
			e.Class, e.SideEffect = provider.ErrRateLimited, provider.EffectNone
			e.RetryAfter = g.pacer.On429(0)
			if ra := retryAfterHeader(gerr.Header); ra > 0 {
				e.RetryAfter = ra
			}
		case gerr.Code == http.StatusForbidden && reasons["quotaExceeded"]:
			e.Class, e.SideEffect = provider.ErrQuota, provider.EffectNone
		case gerr.Code == http.StatusUnauthorized, gerr.Code == http.StatusForbidden:
			// Credential/permission problems are config errors: fail fast
			// (the SDK's closest class to "auth"), never blind-retry.
			e.Class, e.SideEffect = provider.ErrInvalid, provider.EffectNone
		case gerr.Code == http.StatusConflict, gerr.Code == http.StatusPreconditionFailed:
			e.Class, e.SideEffect = provider.ErrConflict, effectIfSent
		case gerr.Code == http.StatusBadRequest:
			e.Class, e.SideEffect = provider.ErrInvalid, provider.EffectNone
		case gerr.Code >= 500:
			e.Class, e.SideEffect = provider.ErrRetryable, effectIfSent
		default:
			e.Class, e.SideEffect = provider.ErrRetryable, effectIfSent
		}
		return e
	}
	// Transport-level failure: retryable; side effect unknown if sent.
	return &provider.Error{Class: provider.ErrRetryable, SideEffect: effectIfSent,
		Provider: g.instance, Message: err.Error(), Cause: err}
}

func retryAfterHeader(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	if secs, err := strconv.Atoi(h.Get("Retry-After")); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}
