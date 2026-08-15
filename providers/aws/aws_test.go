package aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
	"github.com/samishal1998/fleetplane/providers/pacing"
)

// --- stub: the narrow ec2API surface, no HTTP mocking ---

type ec2Stub struct {
	describeInstances     func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error)
	runInstances          func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error)
	terminateInstances    func(*ec2.TerminateInstancesInput) (*ec2.TerminateInstancesOutput, error)
	describeImages        func(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error)
	describeInstanceTypes func(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error)
	describeRegions       func(*ec2.DescribeRegionsInput) (*ec2.DescribeRegionsOutput, error)

	runCalls           int
	describeTypesCalls int
}

func (s *ec2Stub) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if s.describeInstances == nil {
		return nil, errors.New("unexpected DescribeInstances")
	}
	return s.describeInstances(in)
}

func (s *ec2Stub) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	s.runCalls++
	if s.runInstances == nil {
		return nil, errors.New("unexpected RunInstances")
	}
	return s.runInstances(in)
}

func (s *ec2Stub) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if s.terminateInstances == nil {
		return nil, errors.New("unexpected TerminateInstances")
	}
	return s.terminateInstances(in)
}

func (s *ec2Stub) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	if s.describeImages == nil {
		return nil, errors.New("unexpected DescribeImages")
	}
	return s.describeImages(in)
}

func (s *ec2Stub) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	s.describeTypesCalls++
	if s.describeInstanceTypes == nil {
		return nil, errors.New("unexpected DescribeInstanceTypes")
	}
	return s.describeInstanceTypes(in)
}

func (s *ec2Stub) DescribeRegions(_ context.Context, in *ec2.DescribeRegionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	if s.describeRegions == nil {
		return nil, errors.New("unexpected DescribeRegions")
	}
	return s.describeRegions(in)
}

// apiErr mimics a smithy-go coded API error.
type apiErr struct{ code, msg string }

func (e apiErr) Error() string        { return e.code + ": " + e.msg }
func (e apiErr) ErrorCode() string    { return e.code }
func (e apiErr) ErrorMessage() string { return e.msg }

// --- fixtures ---

func newTestDriver(api ec2API) *AWS {
	return &AWS{
		api: api, instance: "aws-test", ownerID: "own_test", region: "us-east-1",
		pacer:    pacing.New(1000, 1000, 10, nil),
		subnetID: "subnet-11", sgIDs: []string{"sg-1", "sg-2"},
		keyName: "fleet-key", instanceProfile: "fleet-profile",
	}
}

func identity(opID string) map[string]string {
	return provider.IdentityLabels("own_test", "res_test", opID)
}

func inst(id, state string, labels map[string]string) ec2types.Instance {
	var tags []ec2types.Tag
	for k, v := range labels {
		tags = append(tags, ec2types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return ec2types.Instance{
		InstanceId:       awssdk.String(id),
		State:            &ec2types.InstanceState{Name: ec2types.InstanceStateName(state)},
		InstanceType:     ec2types.InstanceType("t3.micro"),
		PublicIpAddress:  awssdk.String("198.51.100.7"),
		Ipv6Address:      awssdk.String("2001:db8::7"),
		PrivateIpAddress: awssdk.String("10.0.0.7"),
		Tags:             tags,
	}
}

func reservations(instances ...ec2types.Instance) *ec2.DescribeInstancesOutput {
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{Instances: instances}},
	}
}

func typesOutput() *ec2.DescribeInstanceTypesOutput {
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: []ec2types.InstanceTypeInfo{{
		InstanceType: ec2types.InstanceType("t3.micro"),
		VCpuInfo:     &ec2types.VCpuInfo{DefaultVCpus: awssdk.Int32(2)},
		MemoryInfo:   &ec2types.MemoryInfo{SizeInMiB: awssdk.Int64(1024)},
	}}}
}

func filterValues(filters []ec2types.Filter, name string) []string {
	for _, f := range filters {
		if f.Name != nil && *f.Name == name {
			return f.Values
		}
	}
	return nil
}

func planCreate(t *testing.T, d *AWS, opID string, spec compute.MachineSpec) provider.Action {
	t.Helper()
	raw, _ := json.Marshal(spec)
	plan, err := d.Plan(context.Background(), provider.PlanRequest{
		ResourceID: "res_test",
		Desired: &provider.DesiredState{
			Name: "ci-1", Spec: raw, Labels: identity(opID),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	action := plan.Actions[0]
	action.ActionID = opID
	return action
}

// --- create ---

func TestAWSCreate_TagsWiringAndOpData(t *testing.T) {
	var lastRun *ec2.RunInstancesInput
	var dedupFilters []ec2types.Filter
	stub := &ec2Stub{
		describeInstances: func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			dedupFilters = in.Filters
			return &ec2.DescribeInstancesOutput{}, nil // dedup miss
		},
		describeImages: func(in *ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
			return &ec2.DescribeImagesOutput{Images: []ec2types.Image{
				{ImageId: awssdk.String("ami-old"), CreationDate: awssdk.String("2026-01-01T00:00:00.000Z")},
				{ImageId: awssdk.String("ami-new"), CreationDate: awssdk.String("2026-06-01T00:00:00.000Z")},
			}}, nil
		},
		runInstances: func(in *ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
			lastRun = in
			return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{inst("i-abc123", "pending", nil)}}, nil
		},
	}
	d := newTestDriver(stub)

	action := planCreate(t, d, "op_fresh", compute.MachineSpec{
		ServerType: "t3.micro", Image: "snapshot:purpose=ci",
		Location: "us-east-1a", UserData: "#!/bin/sh\necho hi",
		Labels: map[string]string{"role": "ci"},
	})
	ref, err := d.Apply(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Ref == nil || ref.Ref.ID != "i-abc123" {
		t.Fatalf("ref = %+v", ref.Ref)
	}
	if stub.runCalls != 1 {
		t.Fatalf("RunInstances calls = %d", stub.runCalls)
	}

	// The dedup search must key on the op tag among non-terminated states.
	if got := filterValues(dedupFilters, "tag:"+provider.LabelOp); len(got) != 1 || got[0] != "op_fresh" {
		t.Fatalf("dedup op filter = %v", got)
	}
	states := filterValues(dedupFilters, "instance-state-name")
	if strings.Join(states, ",") != "pending,running,stopping,stopped" {
		t.Fatalf("dedup state filter = %v", states)
	}

	// MachineSpec mapping.
	if lastRun.InstanceType != ec2types.InstanceType("t3.micro") {
		t.Fatalf("instance type = %q", lastRun.InstanceType)
	}
	if *lastRun.ImageId != "ami-new" {
		t.Fatalf("snapshot newest-wins failed: image = %q", *lastRun.ImageId)
	}
	if lastRun.Placement == nil || *lastRun.Placement.AvailabilityZone != "us-east-1a" {
		t.Fatalf("placement = %+v", lastRun.Placement)
	}
	if *lastRun.UserData != base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\necho hi")) {
		t.Fatalf("user data not base64: %q", *lastRun.UserData)
	}
	if *lastRun.SubnetId != "subnet-11" || len(lastRun.SecurityGroupIds) != 2 ||
		*lastRun.KeyName != "fleet-key" || *lastRun.IamInstanceProfile.Name != "fleet-profile" {
		t.Fatalf("settings wiring: subnet=%v sgs=%v key=%v profile=%+v",
			lastRun.SubnetId, lastRun.SecurityGroupIds, lastRun.KeyName, lastRun.IamInstanceProfile)
	}
	if *lastRun.ClientToken != "op_fresh" {
		t.Fatalf("client token = %q", *lastRun.ClientToken)
	}

	// Identity labels ride as tags VERBATIM (dots and slashes intact),
	// plus Name from the desired name and the kind-spec labels.
	if len(lastRun.TagSpecifications) != 1 || lastRun.TagSpecifications[0].ResourceType != ec2types.ResourceTypeInstance {
		t.Fatalf("tag specifications = %+v", lastRun.TagSpecifications)
	}
	got := map[string]string{}
	for _, tag := range lastRun.TagSpecifications[0].Tags {
		got[*tag.Key] = *tag.Value
	}
	want := identity("op_fresh")
	want["Name"] = "ci-1"
	want["role"] = "ci"
	if len(got) != len(want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("tag %q = %q, want %q", k, got[k], v)
		}
	}

	// The frozen op-data shape.
	if string(ref.Data) != `{"v":1,"instanceId":"i-abc123"}` {
		t.Fatalf("op data = %s", ref.Data)
	}
}

func TestAWSCreate_OpTagDedup(t *testing.T) {
	stub := &ec2Stub{
		describeInstances: func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			if got := filterValues(in.Filters, "tag:"+provider.LabelOp); len(got) == 1 && got[0] == "op_replay" {
				return reservations(inst("i-existing", "running", identity("op_replay"))), nil
			}
			return &ec2.DescribeInstancesOutput{}, nil
		},
	}
	d := newTestDriver(stub)

	action := planCreate(t, d, "op_replay", compute.MachineSpec{ServerType: "t3.micro", Image: "id:ami-42"})
	ref, err := d.Apply(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if stub.runCalls != 0 {
		t.Fatalf("dedup failed: %d RunInstances calls", stub.runCalls)
	}
	if ref.Ref == nil || ref.Ref.ID != "i-existing" {
		t.Fatalf("ref = %+v", ref.Ref)
	}
}

// --- image resolution ---

func TestAWSImageResolution(t *testing.T) {
	ctx := context.Background()

	t.Run("IDPassesThroughWithoutAPICall", func(t *testing.T) {
		d := newTestDriver(&ec2Stub{}) // any API call would error
		got, err := d.image(ctx, "id:ami-42")
		if err != nil || got != "ami-42" {
			t.Fatalf("image = %q, %v", got, err)
		}
	})

	t.Run("NamePatternSelfAndAmazonNewestWins", func(t *testing.T) {
		var lastIn *ec2.DescribeImagesInput
		d := newTestDriver(&ec2Stub{
			describeImages: func(in *ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
				lastIn = in
				return &ec2.DescribeImagesOutput{Images: []ec2types.Image{
					{ImageId: awssdk.String("ami-2"), CreationDate: awssdk.String("2026-06-01T00:00:00.000Z")},
					{ImageId: awssdk.String("ami-1"), CreationDate: awssdk.String("2026-01-01T00:00:00.000Z")},
				}}, nil
			},
		})
		got, err := d.image(ctx, "name:ubuntu/images/*24.04*")
		if err != nil || got != "ami-2" {
			t.Fatalf("image = %q, %v", got, err)
		}
		if strings.Join(lastIn.Owners, ",") != "self,amazon" {
			t.Fatalf("owners = %v", lastIn.Owners)
		}
		if got := filterValues(lastIn.Filters, "name"); len(got) != 1 || got[0] != "ubuntu/images/*24.04*" {
			t.Fatalf("name filter = %v", got)
		}
		if got := filterValues(lastIn.Filters, "state"); len(got) != 1 || got[0] != "available" {
			t.Fatalf("state filter = %v", got)
		}
	})

	t.Run("SnapshotTagSelectorSelfOwned", func(t *testing.T) {
		var lastIn *ec2.DescribeImagesInput
		d := newTestDriver(&ec2Stub{
			describeImages: func(in *ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
				lastIn = in
				return &ec2.DescribeImagesOutput{Images: []ec2types.Image{
					{ImageId: awssdk.String("ami-snap"), CreationDate: awssdk.String("2026-03-01T00:00:00.000Z")},
				}}, nil
			},
		})
		got, err := d.image(ctx, "snapshot:purpose=ci")
		if err != nil || got != "ami-snap" {
			t.Fatalf("image = %q, %v", got, err)
		}
		if strings.Join(lastIn.Owners, ",") != "self" {
			t.Fatalf("owners = %v", lastIn.Owners)
		}
		if got := filterValues(lastIn.Filters, "tag:purpose"); len(got) != 1 || got[0] != "ci" {
			t.Fatalf("tag filter = %v", got)
		}
	})

	t.Run("NoMatchFailsFastAsInvalid", func(t *testing.T) {
		d := newTestDriver(&ec2Stub{
			describeImages: func(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
				return &ec2.DescribeImagesOutput{}, nil
			},
		})
		_, err := d.image(ctx, "snapshot:purpose=nope")
		if !provider.IsClass(err, provider.ErrInvalid) || provider.Effect(err) != provider.EffectNone {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("MalformedSelectorsAreInvalidEffectNone", func(t *testing.T) {
		d := newTestDriver(&ec2Stub{})
		for _, spec := range []string{"plainstring", "flavor:x", "id:notami", "snapshot:noequals", "name:"} {
			_, err := d.image(ctx, spec)
			if !provider.IsClass(err, provider.ErrInvalid) {
				t.Fatalf("image(%q) err = %v, want ErrInvalid", spec, err)
			}
			if provider.Effect(err) != provider.EffectNone {
				t.Fatalf("image(%q) effect = maybe, want none", spec)
			}
		}
	})
}

// --- get ---

func TestAWSGet(t *testing.T) {
	t.Run("NotFoundCode", func(t *testing.T) {
		d := newTestDriver(&ec2Stub{
			describeInstances: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
				return nil, apiErr{"InvalidInstanceID.NotFound", "i-gone does not exist"}
			},
		})
		_, err := d.Get(context.Background(), provider.ExternalRef{ID: "i-gone"})
		if !provider.IsClass(err, provider.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if provider.Effect(err) != provider.EffectNone {
			t.Fatal("Get not_found must be EffectNone")
		}
	})

	t.Run("EmptyResultSynthesizesNotFound", func(t *testing.T) {
		d := newTestDriver(&ec2Stub{
			describeInstances: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
				return &ec2.DescribeInstancesOutput{}, nil
			},
		})
		_, err := d.Get(context.Background(), provider.ExternalRef{ID: "i-void"})
		if !provider.IsClass(err, provider.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("TerminatedIsGoneNotError", func(t *testing.T) {
		d := newTestDriver(&ec2Stub{
			describeInstances: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
				return reservations(inst("i-dead", "terminated", identity("op_x"))), nil
			},
			describeInstanceTypes: func(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
				return typesOutput(), nil
			},
		})
		obs, err := d.Get(context.Background(), provider.ExternalRef{ID: "i-dead"})
		if err != nil {
			t.Fatal(err)
		}
		if obs.Phase != provider.PhaseGone || obs.ProviderState != "terminated" {
			t.Fatalf("obs = %+v", obs)
		}
	})
}

// --- delete ---

func TestAWSDeleteOfDeletedIsSuccess(t *testing.T) {
	d := newTestDriver(&ec2Stub{
		terminateInstances: func(*ec2.TerminateInstancesInput) (*ec2.TerminateInstancesOutput, error) {
			return nil, apiErr{"InvalidInstanceID.NotFound", "already gone"}
		},
	})
	ref, err := d.Apply(context.Background(), provider.Action{
		ActionID: "op_d", Kind: "delete", Ref: &provider.ExternalRef{ID: "i-dead"}, Destructive: true,
	})
	if err != nil {
		t.Fatalf("delete of deleted: %v", err)
	}
	if ref.Ref == nil || ref.Ref.ID != "i-dead" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestAWSDelete_MarksOpDataAsDelete(t *testing.T) {
	var terminated []string
	d := newTestDriver(&ec2Stub{
		terminateInstances: func(in *ec2.TerminateInstancesInput) (*ec2.TerminateInstancesOutput, error) {
			terminated = in.InstanceIds
			return &ec2.TerminateInstancesOutput{}, nil
		},
	})
	ref, err := d.Apply(context.Background(), provider.Action{
		ActionID: "op_d", Kind: "delete", Ref: &provider.ExternalRef{ID: "i-live"}, Destructive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(terminated) != 1 || terminated[0] != "i-live" {
		t.Fatalf("terminated = %v", terminated)
	}
	var data opData
	if err := json.Unmarshal(ref.Data, &data); err != nil || !data.Delete || data.InstanceID != "i-live" {
		t.Fatalf("op data = %s (%v)", ref.Data, err)
	}
}

// --- discover ---

func TestAWSDiscover_OwnedScopeFiltersAndDecode(t *testing.T) {
	labels := identity("op_test")
	labels["role"] = "ci"
	var lastFilters []ec2types.Filter
	stub := &ec2Stub{
		describeInstances: func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			lastFilters = in.Filters
			return reservations(inst("i-abc123", "running", labels)), nil
		},
		describeInstanceTypes: func(in *ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
			return typesOutput(), nil
		},
	}
	d := newTestDriver(stub)

	out, err := d.Discover(context.Background(), provider.DiscoverRequest{
		Scope: provider.ScopeOwned, Selector: map[string]string{"role": "ci"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Owned scope pushes ownership + selector tags server-side and never
	// lists terminated ghosts.
	if got := filterValues(lastFilters, "tag:"+provider.LabelManaged); len(got) != 1 || got[0] != "true" {
		t.Fatalf("managed filter = %v", got)
	}
	if got := filterValues(lastFilters, "tag:"+provider.LabelOwner); len(got) != 1 || got[0] != "own_test" {
		t.Fatalf("owner filter = %v", got)
	}
	if got := filterValues(lastFilters, "tag:role"); len(got) != 1 || got[0] != "ci" {
		t.Fatalf("selector filter = %v", got)
	}
	states := filterValues(lastFilters, "instance-state-name")
	for _, s := range states {
		if s == "terminated" {
			t.Fatalf("discover must not list terminated instances: %v", states)
		}
	}

	if len(out) != 1 {
		t.Fatalf("discovered %d instances", len(out))
	}
	obs := out[0]
	if !obs.Owned || obs.FleetplaneID != "res_test" || obs.CreateOpID != "op_test" {
		t.Fatalf("identity not decoded from tags: %+v", obs)
	}
	// Label decode round-trip: tags come back verbatim.
	for k, v := range labels {
		if obs.Labels[k] != v {
			t.Fatalf("label %q = %q, want %q", k, obs.Labels[k], v)
		}
	}
	if obs.Capacity[compute.DimCPU] != 2 || obs.Capacity[compute.DimMemoryMiB] != 1024 {
		t.Fatalf("capacity = %v", obs.Capacity)
	}
	// Address networks use the probe-visible enum from provider.Address:
	// "public-v4" | "public-v6" | "private" (pkg/kinds/compute/probe.go).
	seen := map[string]string{}
	for _, a := range obs.Addresses {
		seen[a.Network] = a.Addr
	}
	if seen["public-v4"] != "198.51.100.7" || seen["public-v6"] != "2001:db8::7" || seen["private"] != "10.0.0.7" {
		t.Fatalf("addresses = %v", obs.Addresses)
	}
	if len(obs.Extensions) == 0 {
		t.Fatal("extensions missing (invariant 6)")
	}

	// Capacity is cached per instance type: a second sweep must not hit
	// DescribeInstanceTypes again.
	if _, err := d.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeOwned}); err != nil {
		t.Fatal(err)
	}
	if stub.describeTypesCalls != 1 {
		t.Fatalf("DescribeInstanceTypes calls = %d, want 1 (cache)", stub.describeTypesCalls)
	}
}

// TestAWSDiscover_ForeignNeverDecodesAsOwned: under ScopeAll, instances
// without our identity tags — or carrying ANOTHER control plane's owner —
// must come back Owned=false with no FleetplaneID (checklist item 5).
func TestAWSDiscover_ForeignNeverDecodesAsOwned(t *testing.T) {
	foreignLabels := map[string]string{
		provider.LabelManaged: "true",
		provider.LabelOwner:   "own_SOMEBODY_ELSE",
		provider.LabelID:      "res_theirs",
		provider.LabelOp:      "op_theirs",
	}
	d := newTestDriver(&ec2Stub{
		describeInstances: func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			// ScopeAll must not push ownership tag filters server-side.
			if got := filterValues(in.Filters, "tag:"+provider.LabelOwner); got != nil {
				t.Fatalf("ScopeAll pushed owner filter: %v", got)
			}
			return reservations(
				inst("i-bare", "running", nil),             // untagged foreign
				inst("i-theirs", "running", foreignLabels), // other control plane
			), nil
		},
		describeInstanceTypes: func(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
			return typesOutput(), nil
		},
	})
	out, err := d.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("discovered %d instances", len(out))
	}
	for _, obs := range out {
		if obs.Owned {
			t.Fatalf("foreign instance %s decoded as owned", obs.Ref.ID)
		}
	}
	if out[0].FleetplaneID != "" || out[0].CreateOpID != "" {
		t.Fatalf("untagged instance decoded identity: %+v", out[0])
	}
	// Foreign identity tags are still visible as labels (adoption flows read
	// them) — they just never grant ownership under a different owner ID.
	if out[1].FleetplaneID != "res_theirs" || out[1].Owned {
		t.Fatalf("other-owner decode = %+v", out[1])
	}
}

// --- observe operation ---

func TestAWSObserveOperation(t *testing.T) {
	createData, _ := json.Marshal(opData{V: 1, InstanceID: "i-abc123"})
	deleteData, _ := json.Marshal(opData{V: 1, InstanceID: "i-abc123", Delete: true})
	ref := &provider.ExternalRef{ID: "i-abc123"}

	observeWith := func(state string) *AWS {
		return newTestDriver(&ec2Stub{
			describeInstances: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
				return reservations(inst("i-abc123", state, identity("op_test"))), nil
			},
			describeInstanceTypes: func(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
				return typesOutput(), nil
			},
		})
	}

	t.Run("CreateRunningSucceeds", func(t *testing.T) {
		status, err := observeWith("running").ObserveOperation(context.Background(),
			provider.OperationRef{ActionID: "op_test", Ref: ref, Data: createData})
		if err != nil {
			t.Fatal(err)
		}
		if status.State != provider.OpSucceeded || status.Resource == nil {
			t.Fatalf("status = %+v", status)
		}
		if len(status.Resource.Extensions) == 0 {
			t.Fatal("extensions missing (invariant 6)")
		}
	})

	t.Run("CreatePendingKeepsPolling", func(t *testing.T) {
		status, err := observeWith("pending").ObserveOperation(context.Background(),
			provider.OperationRef{ActionID: "op_test", Ref: ref, Data: createData})
		if err != nil {
			t.Fatal(err)
		}
		if status.State != provider.OpRunning {
			t.Fatalf("status = %+v", status)
		}
	})

	t.Run("CreateVanishedIsNotFound", func(t *testing.T) {
		d := newTestDriver(&ec2Stub{
			describeInstances: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
				return nil, apiErr{"InvalidInstanceID.NotFound", "gone"}
			},
		})
		_, err := d.ObserveOperation(context.Background(),
			provider.OperationRef{ActionID: "op_test", Ref: ref, Data: createData})
		if !provider.IsClass(err, provider.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("CreateTerminatedStillVisibleIsNotFound", func(t *testing.T) {
		_, err := observeWith("terminated").ObserveOperation(context.Background(),
			provider.OperationRef{ActionID: "op_test", Ref: ref, Data: createData})
		if !provider.IsClass(err, provider.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("DeleteTerminatedSucceedsWithGoneResource", func(t *testing.T) {
		status, err := observeWith("terminated").ObserveOperation(context.Background(),
			provider.OperationRef{ActionID: "op_test", Ref: ref, Data: deleteData})
		if err != nil {
			t.Fatal(err)
		}
		if status.State != provider.OpSucceeded || status.Resource == nil || status.Resource.Phase != provider.PhaseGone {
			t.Fatalf("status = %+v", status)
		}
	})

	t.Run("DeleteShuttingDownKeepsPolling", func(t *testing.T) {
		status, err := observeWith("shutting-down").ObserveOperation(context.Background(),
			provider.OperationRef{ActionID: "op_test", Ref: ref, Data: deleteData})
		if err != nil {
			t.Fatal(err)
		}
		if status.State != provider.OpRunning {
			t.Fatalf("status = %+v", status)
		}
	})
}

// --- billing + descriptor contract ---

func TestAWSBillingPolicyContract(t *testing.T) {
	d := newTestDriver(&ec2Stub{})
	var ba provider.BillingAware = d // compile-time capability check

	pol := ba.Billing(compute.Kind)
	if pol.MinimumDuration != time.Minute || pol.BillingIncrement != 0 {
		t.Fatalf("compute.machine policy = %+v", pol)
	}
	if und := ba.Billing(provider.ResourceKind("storage.volume")); !und.FineGrained() {
		t.Fatalf("undeclared kind policy = %+v, want zero", und)
	}

	desc := d.Descriptor()
	if desc.Driver != "aws" || !desc.SupportsLabelDiscovery {
		t.Fatalf("descriptor = %+v", desc)
	}
	if len(desc.Kinds) != 1 || desc.Kinds[0] != compute.Kind {
		t.Fatalf("kinds = %v", desc.Kinds)
	}
	if _, ok := d.ResourceDriver(compute.Kind); !ok {
		t.Fatal("compute.machine driver missing")
	}
	if _, ok := d.ResourceDriver("storage.volume"); ok {
		t.Fatal("undeclared kind must not resolve a driver")
	}
}

func TestAWSRegistered(t *testing.T) {
	for _, name := range provider.Drivers() {
		if name == Driver {
			return
		}
	}
	t.Fatalf("driver %q not registered: %v", Driver, provider.Drivers())
}

// --- error mapping ---

func TestAWSErrorMapping(t *testing.T) {
	cases := []struct {
		code   string
		class  provider.ErrorClass
		effect provider.SideEffect
	}{
		{"Throttling", provider.ErrRateLimited, provider.EffectNone},
		{"RequestLimitExceeded", provider.ErrRateLimited, provider.EffectNone},
		{"RequestThrottled", provider.ErrRateLimited, provider.EffectNone},
		{"UnauthorizedOperation", provider.ErrInvalid, provider.EffectNone},
		{"AuthFailure", provider.ErrInvalid, provider.EffectNone},
		{"InstanceLimitExceeded", provider.ErrQuota, provider.EffectNone},
		{"VcpuLimitExceeded", provider.ErrQuota, provider.EffectNone},
		{"InvalidInstanceID.NotFound", provider.ErrNotFound, provider.EffectNone},
		{"InvalidAMIID.NotFound", provider.ErrNotFound, provider.EffectNone},
		{"InvalidInstanceID.Malformed", provider.ErrInvalid, provider.EffectNone},
		{"InternalError", provider.ErrRetryable, provider.EffectMaybe},
	}
	for _, tc := range cases {
		d := newTestDriver(&ec2Stub{}) // fresh pacer: On429 parks it
		err := d.mapErr(apiErr{tc.code, "boom"}, provider.EffectMaybe)
		if !provider.IsClass(err, tc.class) {
			t.Fatalf("%s: class = %v, want %v", tc.code, provider.Classify(err), tc.class)
		}
		if provider.Effect(err) != tc.effect {
			t.Fatalf("%s: effect = %v, want %v", tc.code, provider.Effect(err), tc.effect)
		}
		var pe *provider.Error
		if !errors.As(err, &pe) || pe.Code != tc.code {
			t.Fatalf("%s: not a typed provider error: %v", tc.code, err)
		}
		if tc.class == provider.ErrRateLimited && pe.RetryAfter <= 0 {
			t.Fatalf("%s: rate-limited without RetryAfter", tc.code)
		}
	}

	// Transport-level failures stay retryable with the caller's effect.
	d := newTestDriver(&ec2Stub{})
	err := d.mapErr(errors.New("connection reset"), provider.EffectMaybe)
	if !provider.IsClass(err, provider.ErrRetryable) || provider.Effect(err) != provider.EffectMaybe {
		t.Fatalf("transport err = %v", err)
	}
}

// TestAWSApply_PreflightFailureIsEffectNone: a mutation aborted before any
// request leaves the driver (pacer gating / canceled context) must be
// EffectNone — blind re-execution is provably safe (checklist item 2).
func TestAWSApply_PreflightFailureIsEffectNone(t *testing.T) {
	stub := &ec2Stub{} // any API call would fail the test via unexpected-call errors
	d := newTestDriver(stub)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pacer.Acquire fails before anything is sent

	action := planCreate(t, d, "op_pre", compute.MachineSpec{ServerType: "t3.micro", Image: "id:ami-42"})
	_, err := d.Apply(ctx, action)
	if err == nil {
		t.Fatal("expected pre-flight failure")
	}
	if provider.Effect(err) != provider.EffectNone {
		t.Fatalf("pre-flight create effect = maybe, want none (err = %v)", err)
	}
	if stub.runCalls != 0 {
		t.Fatalf("RunInstances called %d times after pre-flight failure", stub.runCalls)
	}

	_, err = d.Apply(ctx, provider.Action{
		ActionID: "op_pre_d", Kind: "delete", Ref: &provider.ExternalRef{ID: "i-x"}, Destructive: true,
	})
	if err == nil || provider.Effect(err) != provider.EffectNone {
		t.Fatalf("pre-flight delete effect = maybe, want none (err = %v)", err)
	}
}

// --- constructor / settings ---

func TestAWSNew_SettingsValidation(t *testing.T) {
	ctx := context.Background()
	newWith := func(s Settings) error {
		raw, _ := json.Marshal(s)
		_, err := New(ctx, provider.InstanceConfig{
			Instance: "aws-test", OwnerID: "own_test",
			Settings: raw, Secrets: secretref.NewDefault(),
		})
		return err
	}

	if err := newWith(Settings{}); !provider.IsClass(err, provider.ErrInvalid) {
		t.Fatalf("missing region: err = %v", err)
	}
	if err := newWith(Settings{Region: "us-east-1", AccessKeyID: "AKIA..."}); !provider.IsClass(err, provider.ErrInvalid) {
		t.Fatalf("half a static pair: err = %v", err)
	}
	if err := newWith(Settings{Region: "us-east-1", SessionToken: "tok"}); !provider.IsClass(err, provider.ErrInvalid) {
		t.Fatalf("session token without pair: err = %v", err)
	}

	// Hermetic ambient environment: no user profile or shared files.
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/aws-config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/aws-credentials")
	t.Setenv("AWS_TEST_KEY", "AKIAEXAMPLE")
	t.Setenv("AWS_TEST_SECRET", "secretvalue")
	raw, _ := json.Marshal(Settings{
		Region:          "us-east-1",
		AccessKeyID:     "secret://env/AWS_TEST_KEY",
		SecretAccessKey: "secret://env/AWS_TEST_SECRET",
		Endpoint:        "http://127.0.0.1:0", // never dialed here
	})
	d, err := New(ctx, provider.InstanceConfig{
		Instance: "aws-prod", OwnerID: "own_test",
		Settings: raw, Secrets: secretref.NewDefault(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Descriptor(); got.Driver != "aws" || got.Instance != "aws-prod" {
		t.Fatalf("descriptor = %+v", got)
	}
}
