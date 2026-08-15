package app

import (
	"context"
	"encoding/json"

	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/storage"
)

// AttachReconciler completes the app wiring (built after the Service).
func (s *Service) AttachReconciler(rec *reconcile.Reconciler) { s.rec = rec }

type UpsertPoolCmd struct {
	ID    string // empty on POST: create (or adopt by metadata.name)
	Name  string
	Spec  json.RawMessage // reconcile.PoolSpec shape
	Actor string
}

func (s *Service) UpsertPool(ctx context.Context, cmd UpsertPoolCmd) (*storage.Pool, error) {
	if cmd.Name == "" && cmd.ID == "" {
		return nil, invalid("pool needs metadata.name")
	}
	var spec reconcile.PoolSpec
	if err := json.Unmarshal(cmd.Spec, &spec); err != nil {
		return nil, invalid("pool spec: %v", err)
	}
	if spec.Replicas < 0 {
		return nil, invalid("pool spec: replicas must be >= 0")
	}
	if spec.Class != "" {
		if _, ok := s.classesResolver().Class(spec.Class); !ok {
			return nil, invalid("pool spec: unknown class %q", spec.Class)
		}
	} else if spec.Machine == nil || spec.Provider == "" || spec.Kind == "" {
		return nil, invalid("pool spec needs either class or kind+provider+machine")
	}

	nowMs := s.clock.Now().UnixMilli()
	var pool *storage.Pool
	err := s.st.Tx(ctx, func(tx storage.TxStore) error {
		switch {
		case cmd.ID != "":
			existing, err := tx.Pools().Get(ctx, storage.PoolID(cmd.ID))
			if err != nil {
				return err
			}
			pool = existing
			if cmd.Name != "" {
				pool.Name = cmd.Name
			}
		default:
			if existing, err := tx.Pools().GetByName(ctx, cmd.Name); err == nil {
				pool = existing // POST with an existing name updates it
			} else {
				pool = &storage.Pool{
					ID: storage.PoolID(ids.New(ids.Pool)), Name: cmd.Name,
					Kind: spec.Kind, Generation: 1, CreatedAt: nowMs,
				}
			}
		}
		pool.Spec = cmd.Spec
		pool.UpdatedAt = nowMs
		if err := tx.Pools().Upsert(ctx, pool); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: cmd.Actor, Type: "pool.upserted", PoolID: &pool.ID,
			Intent: cmd.Spec, Outcome: "applied",
		})
	})
	if err != nil {
		return nil, err
	}
	s.rec.Kick(pool.ID)
	return pool, nil
}

func (s *Service) GetPool(ctx context.Context, id string) (*storage.Pool, error) {
	if p, err := s.st.Pools().Get(ctx, storage.PoolID(id)); err == nil {
		return p, nil
	}
	return s.st.Pools().GetByName(ctx, id) // name works too (CLI convenience)
}

func (s *Service) ListPools(ctx context.Context) ([]*storage.Pool, error) {
	return s.st.Pools().List(ctx)
}

func (s *Service) ReconcilePool(ctx context.Context, id string) error {
	pool, err := s.GetPool(ctx, id)
	if err != nil {
		return err
	}
	s.rec.Kick(pool.ID)
	return nil
}

// classesResolver: the scheduler carries the resolver; expose via a tiny
// interface to avoid duplicated wiring.
func (s *Service) classesResolver() reconcile.ClassResolver {
	if s.sched == nil {
		return emptyClasses{}
	}
	return s.sched.Classes()
}

type emptyClasses struct{}

func (emptyClasses) Class(string) (reconcile.Class, bool) { return reconcile.Class{}, false }
