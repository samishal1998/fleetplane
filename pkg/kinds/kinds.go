// Package kinds is the resource-kind registry (02 §3, Phase 9): kind
// modules register their spec validation and capacity dimensions here, and
// the kernel consults the registry instead of knowing any kind by name —
// the core must not know what a VM or a volume is (00 §core design rule).
package kinds

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

type Descriptor struct {
	Kind         provider.ResourceKind
	ValidateSpec func(json.RawMessage) error
	CapacityDims []provider.Dimension
}

var registry = struct {
	mu sync.RWMutex
	m  map[provider.ResourceKind]Descriptor
}{m: map[provider.ResourceKind]Descriptor{}}

// Register registers a kind module (called from the module's init()).
// Panics on duplicates — that is a build mistake.
func Register(d Descriptor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, dup := registry.m[d.Kind]; dup {
		panic(fmt.Sprintf("kinds: %q registered twice", d.Kind))
	}
	registry.m[d.Kind] = d
}

func Get(kind provider.ResourceKind) (Descriptor, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	d, ok := registry.m[kind]
	return d, ok
}

// Validate checks a kind-specific spec against its registered module.
func Validate(kind provider.ResourceKind, spec json.RawMessage) error {
	d, ok := Get(kind)
	if !ok {
		return fmt.Errorf("unknown resource kind %q (registered: %v)", kind, Names())
	}
	if d.ValidateSpec == nil {
		return nil
	}
	return d.ValidateSpec(spec)
}

func Names() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.m))
	for k := range registry.m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}
