// Package aws drives compute.machine on Amazon EC2 via aws-sdk-go-v2.
// SDK retries are capped at one attempt (config.WithRetryMaxAttempts(1)) —
// the operation engine is the only retry authority (ADR-014, invariant 7).
//
// Identity labels ride as native EC2 instance tags: AWS tag keys permit dots
// and slashes, so fleetplane.io/* keys go through VERBATIM — no codec.
//
// Credentials: accessKeyId/secretAccessKey (and optional sessionToken) are
// secret:// references resolved at construction time. When the static pair
// is omitted entirely, the driver falls back to the SDK's ambient credential
// chain (environment, shared config/credentials files, IMDS/IRSA roles).
package aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
	"github.com/samishal1998/fleetplane/providers/pacing"
)

const Driver = "aws"

func init() {
	provider.Register(Driver, func(ctx context.Context, cfg provider.InstanceConfig) (provider.Provider, error) {
		return New(ctx, cfg)
	})
}

// Settings is the driver config block (03 §4). This JSON shape is frozen —
// docs and the config schema are written against it.
type Settings struct {
	Region string `json:"region"` // required

	// Static credentials (secret:// references; all-or-nothing for the
	// pair). Omitted entirely => ambient AWS credential chain.
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	SessionToken    string `json:"sessionToken,omitempty"`

	Endpoint string `json:"endpoint,omitempty"` // test override

	SubnetID         string   `json:"subnetId,omitempty"`
	SecurityGroupIDs []string `json:"securityGroupIds,omitempty"`
	KeyName          string   `json:"keyName,omitempty"`
	InstanceProfile  string   `json:"instanceProfile,omitempty"` // IAM instance profile name

	RPS     float64 `json:"rps,omitempty"`
	Burst   int     `json:"burst,omitempty"`
	MaxConc int     `json:"maxConcurrent,omitempty"`
}

// ec2API is the narrow slice of *ec2.Client this driver calls. EC2's wire
// protocol is awkward to mock over HTTP, so tests stub this interface while
// the constructor wires the real client.
type ec2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeInstanceTypes(ctx context.Context, in *ec2.DescribeInstanceTypesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
}

var _ ec2API = (*ec2.Client)(nil)

type AWS struct {
	api      ec2API
	instance string
	ownerID  string
	region   string
	pacer    *pacing.Pacer

	subnetID        string
	sgIDs           []string
	keyName         string
	instanceProfile string

	// capMu guards capCache: instance-type capacity, cached per type.
	capMu    sync.Mutex
	capCache map[ec2types.InstanceType]provider.Capacity
}

func New(ctx context.Context, cfg provider.InstanceConfig) (*AWS, error) {
	var s Settings
	if len(cfg.Settings) > 0 && string(cfg.Settings) != "null" {
		if err := json.Unmarshal(cfg.Settings, &s); err != nil {
			return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: cfg.Instance, Message: "aws settings: " + err.Error()}
		}
	}
	if s.Region == "" {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "aws: region is required"}
	}
	if (s.AccessKeyID == "") != (s.SecretAccessKey == "") {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "aws: accessKeyId and secretAccessKey are all-or-nothing"}
	}
	if s.SessionToken != "" && s.AccessKeyID == "" {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "aws: sessionToken requires the static accessKeyId/secretAccessKey pair"}
	}

	opts := []func(*config.LoadOptions) error{
		config.WithRegion(s.Region),
		// One attempt total: the engine is the only retry authority (ADR-014).
		config.WithRetryMaxAttempts(1),
	}
	if s.AccessKeyID != "" {
		key, err := resolveSecret(ctx, cfg, s.AccessKeyID)
		if err != nil {
			return nil, err
		}
		secret, err := resolveSecret(ctx, cfg, s.SecretAccessKey)
		if err != nil {
			return nil, err
		}
		session := ""
		if s.SessionToken != "" {
			if session, err = resolveSecret(ctx, cfg, s.SessionToken); err != nil {
				return nil, err
			}
		}
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(key, secret, session)))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "aws config: " + err.Error(), Cause: err}
	}
	client := ec2.NewFromConfig(awsCfg, func(o *ec2.Options) {
		if s.Endpoint != "" {
			o.BaseEndpoint = awssdk.String(s.Endpoint)
		}
	})
	return &AWS{
		api:             client,
		instance:        cfg.Instance,
		ownerID:         cfg.OwnerID,
		region:          s.Region,
		pacer:           pacing.New(s.RPS, s.Burst, s.MaxConc, nil),
		subnetID:        s.SubnetID,
		sgIDs:           s.SecurityGroupIDs,
		keyName:         s.KeyName,
		instanceProfile: s.InstanceProfile,
	}, nil
}

func resolveSecret(ctx context.Context, cfg provider.InstanceConfig, ref string) (string, error) {
	if !secretref.IsRef(ref) {
		return ref, nil
	}
	sec, err := cfg.Secrets.Resolve(ctx, ref)
	if err != nil {
		return "", &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: err.Error()}
	}
	return string(sec.Reveal()), nil
}

// --- provider.Provider ---

// Billing implements provider.BillingAware: EC2 bills per second with a 60s
// minimum (docs/11). Undeclared kinds get the zero (fine-grained) policy —
// the conformance capability contract.
func (d *AWS) Billing(kind provider.ResourceKind) provider.BillingPolicy {
	if kind == compute.Kind {
		return provider.BillingPolicy{MinimumDuration: time.Minute}
	}
	return provider.BillingPolicy{}
}

func (d *AWS) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Driver: Driver, Instance: d.instance, Version: "aws-sdk-go-v2",
		Kinds:                  []provider.ResourceKind{compute.Kind},
		SupportsLabelDiscovery: true,
	}
}

func (d *AWS) Capabilities(context.Context) ([]provider.CapabilityID, error) {
	return []provider.CapabilityID{"compute.machine.create"}, nil
}

func (d *AWS) ResourceDriver(kind provider.ResourceKind) (provider.ResourceDriver, bool) {
	if kind != compute.Kind {
		return nil, false
	}
	return d, true
}

func (d *AWS) Health(ctx context.Context) error {
	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return d.preflight(err)
	}
	defer release()
	// Cheap authenticated read.
	_, err = d.api.DescribeRegions(ctx, &ec2.DescribeRegionsInput{RegionNames: []string{d.region}})
	if err != nil {
		return d.mapErr(err, provider.EffectNone)
	}
	return nil
}

func (d *AWS) Close() error { return nil }

// --- provider.ResourceDriver ---

func (d *AWS) Kind() provider.ResourceKind { return compute.Kind }

// liveStates are the instance states Discover reports. Terminated instances
// linger visibly for up to an hour on EC2; they are PhaseGone artifacts, not
// resources — reporting them would hand the engine's create-verify a dead
// dedup anchor (invariant 7 wants a NON-terminated instance).
var liveStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

// dedupStates are the states a create-dedup hit may be in: an instance that
// is shutting down or terminated is not a usable result of a prior create.
var dedupStates = []string{"pending", "running", "stopping", "stopped"}

func (d *AWS) Discover(ctx context.Context, req provider.DiscoverRequest) ([]provider.ObservedResource, error) {
	filters := []ec2types.Filter{{
		Name: awssdk.String("instance-state-name"), Values: liveStates,
	}}
	if req.Scope != provider.ScopeAll {
		filters = append(filters,
			tagFilter(provider.LabelManaged, "true"),
			tagFilter(provider.LabelOwner, d.ownerID))
	}
	for _, k := range sortedKeys(req.Selector) {
		filters = append(filters, tagFilter(k, req.Selector[k]))
	}
	instances, err := d.describeAll(ctx, filters)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ObservedResource, 0, len(instances))
	for i := range instances {
		out = append(out, d.observe(ctx, &instances[i]))
	}
	return out, nil
}

func tagFilter(key, value string) ec2types.Filter {
	return ec2types.Filter{Name: awssdk.String("tag:" + key), Values: []string{value}}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// describeAll pages DescribeInstances to completion, one paced call per page.
func (d *AWS) describeAll(ctx context.Context, filters []ec2types.Filter) ([]ec2types.Instance, error) {
	var instances []ec2types.Instance
	var next *string
	for {
		release, err := d.pacer.Acquire(ctx)
		if err != nil {
			return nil, d.preflight(err)
		}
		out, err := d.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: filters, MaxResults: awssdk.Int32(1000), NextToken: next,
		})
		release()
		if err != nil {
			return nil, d.mapErr(err, provider.EffectNone)
		}
		for _, r := range out.Reservations {
			instances = append(instances, r.Instances...)
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		next = out.NextToken
	}
	return instances, nil
}

func (d *AWS) Get(ctx context.Context, ref provider.ExternalRef) (provider.ObservedResource, error) {
	if ref.ID == "" {
		return provider.ObservedResource{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance, Message: "empty instance id"}
	}
	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return provider.ObservedResource{}, d.preflight(err)
	}
	out, err := d.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{ref.ID}})
	release()
	if err != nil {
		return provider.ObservedResource{}, d.mapErr(err, provider.EffectNone)
	}
	for _, r := range out.Reservations {
		for i := range r.Instances {
			// A terminated instance is still visible for a while: report
			// it as PhaseGone (Get on missing IDs is ErrNotFound instead).
			return d.observe(ctx, &r.Instances[i]), nil
		}
	}
	return provider.ObservedResource{}, d.notFound(ref.ID)
}

func (d *AWS) Plan(_ context.Context, req provider.PlanRequest) (provider.Plan, error) {
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
			Summary: []string{fmt.Sprintf("create ec2 instance %q (%s, %s)", req.Desired.Name, spec.ServerType, spec.Image)},
		}, nil
	case req.Desired == nil && req.Observed != nil:
		return provider.Plan{
			Actions: []provider.Action{{Kind: "delete", ResourceID: req.ResourceID, Ref: &req.Observed.Ref, Destructive: true}},
			Summary: []string{"delete ec2 instance " + req.Observed.Ref.ID},
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

// opData is OperationRef.Data (self-versioned; drivers tolerate nil). A
// create's Data serializes exactly as {"v":1,"instanceId":"i-..."}.
type opData struct {
	V          int    `json:"v"`
	InstanceID string `json:"instanceId"`
	Delete     bool   `json:"delete,omitempty"`
}

func (d *AWS) Apply(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
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

func (d *AWS) applyCreate(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	var p createParams
	if err := json.Unmarshal(action.Params, &p); err != nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance, Message: "malformed create params: " + err.Error()}
	}

	// Op-tag dedup (invariant 7): a replayed create returns the instance the
	// SAME operation already produced — searched among non-terminated states.
	if existing, err := d.describeAll(ctx, []ec2types.Filter{
		tagFilter(provider.LabelOp, action.ActionID),
		{Name: awssdk.String("instance-state-name"), Values: dedupStates},
	}); err == nil && len(existing) > 0 && existing[0].InstanceId != nil {
		return provider.OperationRef{
			ActionID: action.ActionID,
			Ref:      &provider.ExternalRef{ID: *existing[0].InstanceId},
		}, nil
	}

	imageID, err := d.image(ctx, p.Spec.Image)
	if err != nil {
		return provider.OperationRef{}, err
	}

	input := &ec2.RunInstancesInput{
		MinCount:     awssdk.Int32(1),
		MaxCount:     awssdk.Int32(1),
		ImageId:      awssdk.String(imageID),
		InstanceType: ec2types.InstanceType(p.Spec.ServerType),
		// The op ID doubles as EC2's native idempotency token.
		ClientToken:       awssdk.String(action.ActionID),
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInstance, Tags: createTags(p.Name, p.Spec.Labels, p.Labels)}},
	}
	if p.Spec.Location != "" {
		input.Placement = &ec2types.Placement{AvailabilityZone: awssdk.String(p.Spec.Location)}
	}
	if p.Spec.UserData != "" {
		input.UserData = awssdk.String(base64.StdEncoding.EncodeToString([]byte(p.Spec.UserData)))
	}
	if d.subnetID != "" {
		input.SubnetId = awssdk.String(d.subnetID)
	}
	if len(d.sgIDs) > 0 {
		input.SecurityGroupIds = d.sgIDs
	}
	if d.keyName != "" {
		input.KeyName = awssdk.String(d.keyName)
	}
	if d.instanceProfile != "" {
		input.IamInstanceProfile = &ec2types.IamInstanceProfileSpecification{Name: awssdk.String(d.instanceProfile)}
	}

	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return provider.OperationRef{}, d.preflight(err)
	}
	defer release()
	out, err := d.api.RunInstances(ctx, input)
	if err != nil {
		// The request was sent: outcome uncertain unless proven otherwise.
		return provider.OperationRef{}, d.mapErr(err, provider.EffectMaybe)
	}
	if len(out.Instances) == 0 || out.Instances[0].InstanceId == nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrRetryable,
			SideEffect: provider.EffectMaybe, Provider: d.instance,
			Message: "RunInstances returned no instance"}
	}
	id := *out.Instances[0].InstanceId
	data, _ := json.Marshal(opData{V: 1, InstanceID: id})
	return provider.OperationRef{
		ActionID: action.ActionID,
		Ref:      &provider.ExternalRef{ID: id},
		Data:     data,
	}, nil
}

// createTags composes instance tags: kind-spec labels, then the Name tag
// from the desired name, then the kernel identity labels — identity wins.
// AWS tag keys permit dots and slashes, so fleetplane.io/* keys pass
// through verbatim.
func createTags(name string, specLabels, identity map[string]string) []ec2types.Tag {
	merged := map[string]string{}
	for k, v := range specLabels {
		merged[k] = v
	}
	if name != "" {
		merged["Name"] = name
	}
	for k, v := range identity {
		merged[k] = v
	}
	tags := make([]ec2types.Tag, 0, len(merged))
	for _, k := range sortedKeys(merged) {
		tags = append(tags, ec2types.Tag{Key: awssdk.String(k), Value: awssdk.String(merged[k])})
	}
	return tags
}

func (d *AWS) applyDelete(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	if action.Ref == nil {
		return provider.OperationRef{}, &provider.Error{Class: provider.ErrInvalid,
			SideEffect: provider.EffectNone, Provider: d.instance, Message: "delete requires a ref"}
	}
	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return provider.OperationRef{}, d.preflight(err)
	}
	defer release()
	_, err = d.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{action.Ref.ID}})
	if err != nil {
		mapped := d.mapErr(err, provider.EffectMaybe)
		if provider.IsClass(mapped, provider.ErrNotFound) {
			// Delete of already-deleted is success (docs/03 §6, FI-7).
			return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref}, nil
		}
		return provider.OperationRef{}, mapped
	}
	data, _ := json.Marshal(opData{V: 1, InstanceID: action.Ref.ID, Delete: true})
	return provider.OperationRef{ActionID: action.ActionID, Ref: action.Ref, Data: data}, nil
}

// ObserveOperation polls the instance by ID (never by listing, 05 §10). EC2
// has no first-class operation objects; the instance state IS the operation
// state. Terminated instances stay visible for up to an hour: a delete op
// succeeds as soon as the state is terminated (reported as PhaseGone), a
// create op whose instance terminated gets the not_found treatment (the
// engine routes create+not_found to verification; once the terminated
// instance ages out, plain 404s take over for deletes too).
func (d *AWS) ObserveOperation(ctx context.Context, op provider.OperationRef) (provider.OperationStatus, error) {
	if op.Ref == nil {
		return provider.OperationStatus{State: provider.OpUnknown}, nil
	}
	var data opData
	if len(op.Data) > 0 {
		_ = json.Unmarshal(op.Data, &data) // tolerate lost/corrupt Data
	}
	obs, err := d.Get(ctx, *op.Ref)
	if err != nil {
		return provider.OperationStatus{}, err // incl. not_found — engine interprets by op kind
	}
	if data.Delete {
		if obs.Phase == provider.PhaseGone {
			return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref, Resource: &obs}, nil
		}
		return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref, RetryAfter: 2 * time.Second}, nil
	}
	switch obs.Phase {
	case provider.PhaseRunning:
		return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref, Resource: &obs}, nil
	case provider.PhaseGone:
		// The create's object has vanished (terminated counts): not_found.
		return provider.OperationStatus{}, d.notFound(op.Ref.ID)
	default:
		return provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref, RetryAfter: 2 * time.Second}, nil
	}
}

// --- image resolution ---

// image resolves "id:ami-..." | "name:<pattern>" | "snapshot:<k=v>".
// name searches self+amazon-owned available images by name pattern;
// snapshot searches self-owned available images by tag equality. Newest
// CreationDate wins in both (ISO-8601 sorts lexically).
func (d *AWS) image(ctx context.Context, spec string) (string, error) {
	kind, rest, ok := strings.Cut(spec, ":")
	if !ok || rest == "" {
		return "", d.badImage(`image must be "id:ami-...", "name:<pattern>" or "snapshot:<k=v>"`)
	}
	switch kind {
	case "id":
		if !strings.HasPrefix(rest, "ami-") {
			return "", d.badImage("malformed image id " + rest + ` (want "ami-...")`)
		}
		return rest, nil
	case "name":
		return d.describeNewestImage(ctx, []string{"self", "amazon"}, ec2types.Filter{
			Name: awssdk.String("name"), Values: []string{rest},
		}, spec)
	case "snapshot":
		k, v, ok := strings.Cut(rest, "=")
		if !ok || k == "" {
			return "", d.badImage(`snapshot image selector must be "snapshot:<k=v>"`)
		}
		return d.describeNewestImage(ctx, []string{"self"}, tagFilter(k, v), spec)
	default:
		return "", d.badImage("unknown image form " + kind)
	}
}

func (d *AWS) describeNewestImage(ctx context.Context, owners []string, filter ec2types.Filter, spec string) (string, error) {
	filters := []ec2types.Filter{filter, {Name: awssdk.String("state"), Values: []string{"available"}}}
	var best *ec2types.Image
	var next *string
	for {
		release, err := d.pacer.Acquire(ctx)
		if err != nil {
			return "", d.preflight(err)
		}
		out, err := d.api.DescribeImages(ctx, &ec2.DescribeImagesInput{
			Owners: owners, Filters: filters, NextToken: next,
		})
		release()
		if err != nil {
			return "", d.mapErr(err, provider.EffectNone)
		}
		for i := range out.Images {
			img := &out.Images[i]
			if img.ImageId == nil {
				continue
			}
			if best == nil || deref(img.CreationDate) > deref(best.CreationDate) {
				best = img
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		next = out.NextToken
	}
	if best == nil {
		// A selector matching nothing is a config error: fail fast, not retry.
		return "", d.badImage("no image matches " + spec)
	}
	return *best.ImageId, nil
}

func (d *AWS) badImage(msg string) error {
	return &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
		Provider: d.instance, Message: msg}
}

// --- mapping ---

func (d *AWS) observe(ctx context.Context, in *ec2types.Instance) provider.ObservedResource {
	labels := map[string]string{}
	for _, t := range in.Tags {
		if t.Key != nil {
			labels[*t.Key] = deref(t.Value)
		}
	}
	// Network names are the probe-visible enum from provider.Address:
	// "public-v4" | "public-v6" | "private" — exactly what the readiness
	// prober iterates (pkg/kinds/compute/probe.go).
	var addrs []provider.Address
	if ip := deref(in.PublicIpAddress); ip != "" {
		addrs = append(addrs, provider.Address{Network: "public-v4", Addr: ip})
	}
	if ip := deref(in.Ipv6Address); ip != "" {
		addrs = append(addrs, provider.Address{Network: "public-v6", Addr: ip})
	}
	if ip := deref(in.PrivateIpAddress); ip != "" {
		addrs = append(addrs, provider.Address{Network: "private", Addr: ip})
	}
	state := ec2types.InstanceStateName("")
	if in.State != nil {
		state = in.State.Name
	}
	ext, _ := json.Marshal(in) // full native object (invariant 6)
	return provider.ObservedResource{
		Ref:           provider.ExternalRef{ID: deref(in.InstanceId)},
		Kind:          compute.Kind,
		FleetplaneID:  labels[provider.LabelID],
		CreateOpID:    labels[provider.LabelOp],
		Owned:         labels[provider.LabelManaged] == "true" && labels[provider.LabelOwner] == d.ownerID,
		Phase:         mapState(state),
		ProviderState: string(state),
		Capacity:      d.capacity(ctx, in.InstanceType),
		Addresses:     addrs,
		Labels:        labels,
		Extensions:    ext,
		ObservedAt:    time.Now(),
	}
}

func mapState(s ec2types.InstanceStateName) provider.ObservedPhase {
	switch s {
	case ec2types.InstanceStateNamePending:
		return provider.PhasePending
	case ec2types.InstanceStateNameRunning:
		return provider.PhaseRunning
	case ec2types.InstanceStateNameStopping, ec2types.InstanceStateNameStopped:
		return provider.PhaseStopped
	case ec2types.InstanceStateNameShuttingDown:
		return provider.PhaseDeleting
	case ec2types.InstanceStateNameTerminated:
		return provider.PhaseGone
	default:
		return provider.PhaseUnknown
	}
}

// capacity resolves cpu/memory dimensions for an instance type, cached per
// type for the driver's lifetime (type definitions are immutable). Capacity
// is best-effort: absent dimensions mean "not tracked", so lookup failures
// degrade to an untracked observation rather than failing the caller.
func (d *AWS) capacity(ctx context.Context, it ec2types.InstanceType) provider.Capacity {
	if it == "" {
		return nil
	}
	d.capMu.Lock()
	cached, ok := d.capCache[it]
	d.capMu.Unlock()
	if ok {
		return copyCapacity(cached)
	}
	release, err := d.pacer.Acquire(ctx)
	if err != nil {
		return nil
	}
	out, err := d.api.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []ec2types.InstanceType{it},
	})
	release()
	if err != nil || len(out.InstanceTypes) == 0 {
		return nil
	}
	info := out.InstanceTypes[0]
	cap := provider.Capacity{}
	if info.VCpuInfo != nil && info.VCpuInfo.DefaultVCpus != nil {
		cap[compute.DimCPU] = int64(*info.VCpuInfo.DefaultVCpus)
	}
	if info.MemoryInfo != nil && info.MemoryInfo.SizeInMiB != nil {
		cap[compute.DimMemoryMiB] = *info.MemoryInfo.SizeInMiB
	}
	d.capMu.Lock()
	if d.capCache == nil {
		d.capCache = map[ec2types.InstanceType]provider.Capacity{}
	}
	d.capCache[it] = cap
	d.capMu.Unlock()
	return copyCapacity(cap)
}

func copyCapacity(in provider.Capacity) provider.Capacity {
	out := make(provider.Capacity, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// smithyAPIError is the coded-error surface of smithy-go API errors,
// matched structurally to keep the import set minimal.
type smithyAPIError interface {
	ErrorCode() string
	ErrorMessage() string
}

// mapErr folds AWS SDK errors into the typed model (03 §7). effectIfSent is
// the SideEffect when the request may have reached the API.
func (d *AWS) mapErr(err error, effectIfSent provider.SideEffect) error {
	var ae smithyAPIError
	if errors.As(err, &ae) {
		e := &provider.Error{Code: ae.ErrorCode(), Message: ae.ErrorMessage(),
			Provider: d.instance, RequestID: requestID(err), Cause: err}
		switch code := ae.ErrorCode(); {
		case code == "Throttling" || code == "ThrottlingException" ||
			code == "RequestLimitExceeded" || code == "RequestThrottled" ||
			code == "RequestThrottledException":
			// Throttled requests are rejected before processing.
			e.Class, e.SideEffect = provider.ErrRateLimited, provider.EffectNone
			e.RetryAfter = d.pacer.On429(0)
		case strings.HasSuffix(code, ".NotFound"): // InvalidInstanceID.NotFound, InvalidAMIID.NotFound, ...
			e.Class, e.SideEffect = provider.ErrNotFound, provider.EffectNone
		case code == "InstanceLimitExceeded" || code == "VcpuLimitExceeded":
			e.Class, e.SideEffect = provider.ErrQuota, provider.EffectNone
		case code == "UnauthorizedOperation" || code == "AuthFailure":
			// No dedicated auth class exists (errors.go); invalid is the
			// non-retryable fit, matching the other in-tree drivers.
			e.Class, e.SideEffect = provider.ErrInvalid, provider.EffectNone
		case code == "IdempotentParameterMismatch":
			e.Class, e.SideEffect = provider.ErrConflict, effectIfSent
		case strings.HasSuffix(code, ".Malformed") || code == "ValidationError" ||
			code == "MissingParameter" || strings.HasPrefix(code, "InvalidParameter"):
			e.Class, e.SideEffect = provider.ErrInvalid, provider.EffectNone
		default:
			e.Class, e.SideEffect = provider.ErrRetryable, effectIfSent
		}
		return e
	}
	// Transport-level failure: retryable; side effect unknown if sent.
	return &provider.Error{Class: provider.ErrRetryable, SideEffect: effectIfSent,
		Provider: d.instance, Message: err.Error(), Cause: err}
}

func requestID(err error) string {
	var r interface{ ServiceRequestID() string }
	if errors.As(err, &r) {
		return r.ServiceRequestID()
	}
	return ""
}

// preflight types an error raised BEFORE any request was sent (pacer gating,
// context cancellation): the mutation provably never reached AWS, so blind
// re-execution is safe — EffectNone (invariant 7 discipline). Pacer "parked"
// errors are already typed (rate_limited/EffectNone) and pass through.
func (d *AWS) preflight(err error) error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return err
	}
	return &provider.Error{Class: provider.ErrRetryable, SideEffect: provider.EffectNone,
		Provider: d.instance, Message: "pre-flight: " + err.Error(), Cause: err}
}

func (d *AWS) notFound(id string) *provider.Error {
	return &provider.Error{Class: provider.ErrNotFound, SideEffect: provider.EffectNone,
		Provider: d.instance, Code: "InvalidInstanceID.NotFound",
		Message: "instance " + id + " not found"}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
