package aws

// This file runs the shared provider conformance catalog (docs/03 §8) —
// including the Parking/CapabilityContract and Parking/StopStartLifecycle
// subtests that prove the docs/12 stop/start contract — against the AWS
// driver over miniEC2, a stateful in-memory EC2 control plane behind the
// ec2API seam. miniEC2 is strongly consistent and synchronous (stops land
// "stopped" immediately); the transitional "stopping"/"pending" predicates
// and the terminated-ghost window are covered by the fixed-state unit tests
// in aws_test.go. It models the docs/12 address caveat faithfully: the
// ephemeral public IP is released at stop and a NEW one is assigned at
// start.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/conformance"
)

type miniInstance struct {
	id    string
	state string // running | stopped (transitions are synchronous)
	pubIP string // released at stop, freshly assigned at start
	tags  map[string]string
}

type miniEC2 struct {
	mu        sync.Mutex
	seq       int
	ipSeq     int
	instances map[string]*miniInstance
}

func newMiniEC2() *miniEC2 { return &miniEC2{instances: map[string]*miniInstance{}} }

func (m *miniEC2) freshIPLocked() string {
	m.ipSeq++
	return fmt.Sprintf("203.0.113.%d", m.ipSeq%250+1)
}

func (m *miniEC2) renderLocked(mi *miniInstance) ec2types.Instance {
	var tags []ec2types.Tag
	for k, v := range mi.tags {
		tags = append(tags, ec2types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	out := ec2types.Instance{
		InstanceId:       awssdk.String(mi.id),
		State:            &ec2types.InstanceState{Name: ec2types.InstanceStateName(mi.state)},
		InstanceType:     ec2types.InstanceType("t3.micro"),
		PrivateIpAddress: awssdk.String("10.0.0.9"),
		Tags:             tags,
	}
	if mi.pubIP != "" {
		out.PublicIpAddress = awssdk.String(mi.pubIP)
	}
	return out
}

func (m *miniEC2) matchesLocked(mi *miniInstance, filters []ec2types.Filter) bool {
	for _, f := range filters {
		name := awssdk.ToString(f.Name)
		switch {
		case name == "instance-state-name":
			if !containsStr(f.Values, mi.state) {
				return false
			}
		case strings.HasPrefix(name, "tag:"):
			if !containsStr(f.Values, mi.tags[strings.TrimPrefix(name, "tag:")]) {
				return false
			}
		default:
			return false // unknown filter: match nothing, fail loudly in tests
		}
	}
	return true
}

func containsStr(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

func (m *miniEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ec2types.Instance
	if len(in.InstanceIds) > 0 {
		for _, id := range in.InstanceIds {
			mi, ok := m.instances[id]
			if !ok {
				// Terminated instances age out immediately in the mini; the
				// still-visible ghost window is unit-tested with fixed stubs.
				return nil, apiErr{"InvalidInstanceID.NotFound", "instance " + id + " does not exist"}
			}
			out = append(out, m.renderLocked(mi))
		}
	} else {
		for _, mi := range m.instances {
			if m.matchesLocked(mi, in.Filters) {
				out = append(out, m.renderLocked(mi))
			}
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: out}}}, nil
}

func (m *miniEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	tags := map[string]string{}
	for _, ts := range in.TagSpecifications {
		for _, tg := range ts.Tags {
			tags[awssdk.ToString(tg.Key)] = awssdk.ToString(tg.Value)
		}
	}
	mi := &miniInstance{
		id:    fmt.Sprintf("i-conf%06d", m.seq),
		state: "running",
		pubIP: m.freshIPLocked(),
		tags:  tags,
	}
	m.instances[mi.id] = mi
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{m.renderLocked(mi)}}, nil
}

func (m *miniEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range in.InstanceIds {
		if _, ok := m.instances[id]; !ok {
			return nil, apiErr{"InvalidInstanceID.NotFound", "instance " + id + " does not exist"}
		}
		delete(m.instances, id)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (m *miniEC2) StopInstances(_ context.Context, in *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range in.InstanceIds {
		mi, ok := m.instances[id]
		if !ok {
			return nil, apiErr{"InvalidInstanceID.NotFound", "instance " + id + " does not exist"}
		}
		mi.state = "stopped"
		mi.pubIP = "" // EC2 releases the ephemeral public IP at stop (docs/12 §3)
	}
	return &ec2.StopInstancesOutput{}, nil
}

func (m *miniEC2) StartInstances(_ context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range in.InstanceIds {
		mi, ok := m.instances[id]
		if !ok {
			return nil, apiErr{"InvalidInstanceID.NotFound", "instance " + id + " does not exist"}
		}
		mi.state = "running"
		mi.pubIP = m.freshIPLocked() // a restarted instance usually has a NEW IP
	}
	return &ec2.StartInstancesOutput{}, nil
}

func (m *miniEC2) DescribeInstanceTypes(_ context.Context, _ *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	return typesOutput(), nil
}

func (m *miniEC2) DescribeImages(_ context.Context, _ *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	return nil, errors.New("unexpected DescribeImages (conformance specs use id: images)")
}

func (m *miniEC2) DescribeRegions(_ context.Context, _ *ec2.DescribeRegionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	return nil, errors.New("unexpected DescribeRegions")
}

func TestAWSConformance(t *testing.T) {
	d := newTestDriver(newMiniEC2())
	conformance.Run(t, conformance.Harness{
		Provider: d,
		Kind:     compute.Kind,
		NewSpec: func(i int) json.RawMessage {
			return json.RawMessage(fmt.Sprintf(`{"serverType":"t3.micro","image":"id:ami-%d"}`, i))
		},
		InvalidSpec: json.RawMessage(`{"serverType":"t3.micro"}`), // image missing
		OwnerID:     "own_test",
	})
}
