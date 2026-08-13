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

	"github.com/samimishal/fleetplane/internal/ids"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
	"github.com/samimishal/fleetplane/pkg/sdk/secretref"
)

// Providers is the provider instance manager: it constructs configured
// instances at boot (resolving secret references exactly once, 07 §4) and
// resolves them for the engine and scheduler.
type Providers struct {
	mu        sync.RWMutex
	instances map[storage.ProviderInstance]provider.Provider
}

// ProviderSpec is one configured instance (internal/config supplies it).
type ProviderSpec struct {
	Name     string
	Driver   string
	Settings json.RawMessage
}

// BuildProviders constructs every configured instance and persists the
// instance records (config references only).
func BuildProviders(ctx context.Context, st storage.Store, specs []ProviderSpec, ownerID string, resolver secretref.Resolver, log *slog.Logger, nowMillis int64) (*Providers, error) {
	p := &Providers{instances: map[storage.ProviderInstance]provider.Provider{}}
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
