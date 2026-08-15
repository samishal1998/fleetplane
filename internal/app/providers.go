// Package app is the application-services layer (plan R14): it composes
// idempotency around kernel commands, owns the provider instance manager,
// and exposes the command/query facade the API layer calls.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
)

// Providers is the provider instance manager: it constructs configured
// instances at boot (resolving secret references exactly once, 07 §4) and
// resolves them for the engine and scheduler.
type Providers struct {
	mu        sync.RWMutex
	instances map[storage.ProviderInstance]provider.Provider
	billing   map[billingKey]resolvedBilling
}

type billingKey struct {
	instance storage.ProviderInstance
	kind     string
}

type resolvedBilling struct {
	policy   provider.BillingPolicy
	adaptive bool
}

// ProviderSpec is one configured instance (internal/config supplies it).
type ProviderSpec struct {
	Name     string
	Driver   string
	Settings json.RawMessage
	Billing  map[string]BillingOverride // kind -> override (docs/11 §3–4)
}

// BillingOverride mirrors config's billing block: pointer fields merge
// over the driver's capability so a partial override never silently
// zeroes unspecified fields (design verification finding).
type BillingOverride struct {
	Disabled          bool
	MinimumDuration   *time.Duration
	Increment         *time.Duration
	TerminationBuffer *time.Duration
	Adaptive          bool
}

// BuildProviders constructs every configured instance and persists the
// instance records (config references only).
func BuildProviders(ctx context.Context, st storage.Store, specs []ProviderSpec, ownerID string, resolver secretref.Resolver, log *slog.Logger, nowMillis int64) (*Providers, error) {
	p := &Providers{
		instances: map[storage.ProviderInstance]provider.Provider{},
		billing:   map[billingKey]resolvedBilling{},
	}
	for _, spec := range specs {
		inst, err := provider.New(ctx, spec.Driver, provider.InstanceConfig{
			Instance: spec.Name,
			OwnerID:  ownerID,
			Settings: spec.Settings,
			Secrets:  resolver,
			Logger:   log.With("provider", spec.Name),
		})
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", spec.Name, err)
		}
		p.instances[storage.ProviderInstance(spec.Name)] = inst
		if err := p.resolveBilling(storage.ProviderInstance(spec.Name), inst, spec.Billing); err != nil {
			return nil, err
		}
		err = st.Tx(ctx, func(tx storage.TxStore) error {
			return tx.Providers().Upsert(ctx, &storage.ProviderInstanceRecord{
				Name: storage.ProviderInstance(spec.Name), Driver: spec.Driver,
				Config: spec.Settings, Enabled: true,
				CreatedAt: nowMillis, UpdatedAt: nowMillis,
			})
		})
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Instance implements operations.Providers.
func (p *Providers) Instance(name storage.ProviderInstance) (provider.Provider, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	inst, ok := p.instances[name]
	return inst, ok
}

// resolveBilling merges config overrides over the driver's BillingAware
// capability (config field wins field-wise; disabled zeroes the policy) and
// validates override kinds against the driver's declared kinds.
func (p *Providers) resolveBilling(name storage.ProviderInstance, inst provider.Provider, overrides map[string]BillingOverride) error {
	declared := map[string]bool{}
	for _, k := range inst.Descriptor().Kinds {
		declared[string(k)] = true
	}
	for kind, ov := range overrides {
		if !declared[kind] {
			return fmt.Errorf("providers.%s.billing: kind %q not served by driver (has %v)", name, kind, inst.Descriptor().Kinds)
		}
		pol := p.driverPolicy(inst, kind)
		if ov.Disabled {
			p.billing[billingKey{name, kind}] = resolvedBilling{} // explicit opt-out
			continue
		}
		if ov.MinimumDuration != nil {
			pol.MinimumDuration = *ov.MinimumDuration
		}
		if ov.Increment != nil {
			pol.BillingIncrement = *ov.Increment
		}
		if ov.TerminationBuffer != nil {
			pol.TerminationBuffer = *ov.TerminationBuffer
		}
		p.billing[billingKey{name, kind}] = resolvedBilling{policy: pol, adaptive: ov.Adaptive}
	}
	return nil
}

func (p *Providers) driverPolicy(inst provider.Provider, kind string) provider.BillingPolicy {
	if ba, ok := inst.(provider.BillingAware); ok {
		return ba.Billing(provider.ResourceKind(kind))
	}
	return provider.BillingPolicy{}
}

// Billing resolves the effective billing policy for an instance+kind
// (config override > driver capability > zero) and whether the adaptive
// termination buffer is enabled. Implements provision.Providers.
func (p *Providers) Billing(name storage.ProviderInstance, kind string) (provider.BillingPolicy, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if rb, ok := p.billing[billingKey{name, kind}]; ok {
		return rb.policy, rb.adaptive
	}
	if inst, ok := p.instances[name]; ok {
		return p.driverPolicy(inst, kind), false
	}
	return provider.BillingPolicy{}, false
}

// Parking resolves the stop/resume capability (docs/12). Implements
// provision.Providers.
func (p *Providers) Parking(name storage.ProviderInstance, kind string) provider.ParkPolicy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if inst, ok := p.instances[name]; ok {
		if pa, ok := inst.(provider.ParkAware); ok {
			return pa.Parking(provider.ResourceKind(kind))
		}
	}
	return provider.ParkPolicy{}
}

// Names lists configured instances, sorted.
func (p *Providers) Names() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.instances))
	for n := range p.instances {
		out = append(out, string(n))
	}
	sort.Strings(out)
	return out
}

func (p *Providers) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, inst := range p.instances {
		_ = inst.Close()
	}
}

// instanceCheckpoint persists the control-plane identity (plan R23):
// minted at first boot, verified on every boot.
type instanceCheckpoint struct {
	OwnerID string `json:"ownerId"`
}

// EnsureOwnerID loads or mints the control-plane OwnerID.
func EnsureOwnerID(ctx context.Context, st storage.Store, nowMillis int64) (string, error) {
	var ownerID string
	err := st.Tx(ctx, func(tx storage.TxStore) error {
		raw, err := tx.Checkpoints().Get(ctx, "instance")
		switch {
		case err == nil:
			var cp instanceCheckpoint
			if err := json.Unmarshal(raw, &cp); err != nil || cp.OwnerID == "" {
				return fmt.Errorf("corrupt instance checkpoint: %s", raw)
			}
			ownerID = cp.OwnerID
			return nil
		case errors.Is(err, storage.ErrNotFound):
			ownerID = ids.New(ids.Owner)
			cp, _ := json.Marshal(instanceCheckpoint{OwnerID: ownerID})
			return tx.Checkpoints().Put(ctx, "instance", cp, nowMillis)
		default:
			return err
		}
	})
	return ownerID, err
}
