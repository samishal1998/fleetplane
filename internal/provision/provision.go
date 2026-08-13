// Package provision is the one create-journaling path shared by the app
// service (API creates) and the pool reconciler: plan (pure) outside the
// transaction, then journal resource row + create operation + event
// together in the caller's transaction (co-creation is what makes
// provisioning rows the invariant-4 counting source, plan R20).
package provision

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/samimishal/fleetplane/internal/ids"
	"github.com/samimishal/fleetplane/internal/phase"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

// Providers resolves configured provider instances (structurally identical
// to operations.Providers).
type Providers interface {
	Instance(name storage.ProviderInstance) (provider.Provider, bool)
}

type CreateSpec struct {
	Kind     string
	Provider string
	Name     string
	Spec     json.RawMessage
	Labels   map[string]string
	Class    string
	PoolID   *storage.PoolID
}

// Prepared carries the planned create, ready to journal.
type Prepared struct {
	Resource *storage.Resource
	Op       *storage.Operation
}

// Prepare mints IDs, composes identity labels (R2) and runs the driver's
// pure Plan. No side effects.
func Prepare(ctx context.Context, providers Providers, ownerID string, cs CreateSpec, nowMillis int64) (*Prepared, error) {
	inst, ok := providers.Instance(storage.ProviderInstance(cs.Provider))
	if !ok {
		return nil, fmt.Errorf("unknown provider instance %q", cs.Provider)
	}
	driver, ok := inst.ResourceDriver(provider.ResourceKind(cs.Kind))
	if !ok {
		return nil, fmt.Errorf("provider %q does not drive kind %q", cs.Provider, cs.Kind)
	}

	resID := storage.ResourceID(ids.New(ids.Resource))
	opID := storage.OperationID(ids.New(ids.Operation))
	name := cs.Name
	if name == "" {
		name = string(resID)
	}

	labels := provider.IdentityLabels(ownerID, string(resID), string(opID))
	if cs.Class != "" {
		labels[provider.LabelClass] = cs.Class
	}
	plan, err := driver.Plan(ctx, provider.PlanRequest{
		ResourceID: string(resID),
		Desired: &provider.DesiredState{
			Name:   name,
			Spec:   cs.Spec,
			Labels: labels,
		},
	})
	if err != nil {
		return nil, err
	}
	if len(plan.Actions) != 1 {
		return nil, fmt.Errorf("driver planned %d actions for a create, want 1", len(plan.Actions))
	}
	action := plan.Actions[0]
	action.ActionID = string(opID) // ActionID := OperationID (R2)
	actionJSON, err := json.Marshal(action)
	if err != nil {
		return nil, err
	}

	return &Prepared{
		Resource: &storage.Resource{
			ID: resID, Name: name, Kind: cs.Kind,
			Provider: storage.ProviderInstance(cs.Provider),
			Class:    cs.Class, PoolID: cs.PoolID,
			Ownership: storage.OwnershipManaged, Phase: phase.Provisioning,
			Spec: cs.Spec, Labels: cs.Labels,
			CreatedAt: nowMillis, UpdatedAt: nowMillis,
		},
		Op: &storage.Operation{
			ID: opID, Kind: storage.OpKindCreate, ResourceID: &resID,
			PoolID:   cs.PoolID,
			Provider: storage.ProviderInstance(cs.Provider),
			Action:   actionJSON, State: storage.OpJournaled,
			CreatedAt: nowMillis, UpdatedAt: nowMillis,
		},
	}, nil
}

// Journal writes the resource row, the journaled operation and the audit
// event inside the caller's open transaction (TxA).
func (p *Prepared) Journal(ctx context.Context, tx storage.TxStore, actor, idemKey string) error {
	if err := tx.Resources().Create(ctx, p.Resource); err != nil {
		return err
	}
	if err := tx.Operations().Append(ctx, p.Op); err != nil {
		return err
	}
	return tx.Events().Append(ctx, &storage.Event{
		TS: p.Resource.CreatedAt, Actor: actor, IdemKey: idemKey, Type: "resource.create",
		ResourceID: &p.Resource.ID, OperationID: &p.Op.ID, PoolID: p.Resource.PoolID,
		Provider: p.Resource.Provider, Intent: p.Resource.Spec, Outcome: "journaled",
	})
}
