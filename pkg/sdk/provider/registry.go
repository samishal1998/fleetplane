package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/samimishal/fleetplane/pkg/sdk/secretref"
)

// InstanceConfig configures one provider instance (docs/03 §4). Settings is
// the raw driver config block; it holds secret references, never values.
type InstanceConfig struct {
	Instance string // e.g. "hetzner-prod"
	OwnerID  string // control-plane identity written into ownership labels
	Settings json.RawMessage
	Secrets  secretref.Resolver
	Logger   *slog.Logger
}

// Factory constructs a provider instance. Secret references are resolved
// here (construction time) and nowhere else.
type Factory func(ctx context.Context, cfg InstanceConfig) (Provider, error)

var registry = struct {
	mu        sync.RWMutex
	factories map[string]Factory
}{factories: map[string]Factory{}}

// Register registers a driver factory. Called from provider package init();
// cmd/fleetplane/modules.go controls which providers are compiled in
// (ADR-013). Panics on duplicate registration — that is a build mistake.
func Register(driver string, f Factory) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, dup := registry.factories[driver]; dup {
		panic(fmt.Sprintf("provider: driver %q registered twice", driver))
	}
	registry.factories[driver] = f
}

// New constructs an instance of a registered driver.
func New(ctx context.Context, driver string, cfg InstanceConfig) (Provider, error) {
	registry.mu.RLock()
	f, ok := registry.factories[driver]
	registry.mu.RUnlock()
	if !ok {
		return nil, &Error{
			Class:      ErrInvalid,
			SideEffect: EffectNone,
			Message:    fmt.Sprintf("unknown provider driver %q (compiled drivers: %v)", driver, Drivers()),
		}
	}
	return f(ctx, cfg)
}

// Drivers lists registered driver names, sorted.
func Drivers() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.factories))
	for name := range registry.factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
