package app

import (
	"context"
	"encoding/json"
	"fmt"

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
	if spec.MinRunning < 0 || spec.MinRunning > spec.Replicas {
		return nil, invalid("pool spec: minRunning must be between 0 and replicas")
	}
	if spec.Reclaim != nil && spec.Reclaim.DeleteAfter.Std() > 0 {
		// Stage-2 deleteAfter is poolless-only: pool fleet size is owned by
		// replicas convergence — applying it here would churn delete/create
		// forever (docs/12 design blocker). Reject loudly over silent-ignore.
		return nil, invalid("pool spec: reclaim.deleteAfter applies to poolless classes only (pool size is owned by replicas)")
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

// SetPoolPaused freezes or resumes convergence. A paused pool keeps the fleet
// exactly as it stands — no creates, no drains, no reclaim — which is what an
// operator wants while investigating. Pause is deliberately not part of the
// pool manifest: `paused` has no "unset", so honouring it in apply would mean
// a manifest without the field silently resumed a paused pool.
func (s *Service) SetPoolPaused(ctx context.Context, id string, paused bool, actor string) error {
	pool, err := s.GetPool(ctx, id)
	if err != nil {
		return err
	}
	if pool.Paused == paused {
		return ErrAlreadyThere
	}
	evType := "pool.resumed"
	if paused {
		evType = "pool.paused"
	}
	nowMs := s.clock.Now().UnixMilli()
	err = s.st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Pools().SetPaused(ctx, pool.ID, paused, nowMs); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: actor, Type: evType, PoolID: &pool.ID, Outcome: "applied",
		})
	})
	if err != nil {
		return err
	}
	if !paused {
		s.rec.Kick(pool.ID) // converge now rather than on the next timer tick
	}
	return nil
}

// DeletePool removes an empty, scaled-to-zero pool. Both gates are re-checked
// inside the mutating transaction. Replicas matter as much as membership: a
// pool with replicas > 0 and no members yet is one the reconciler is actively
// creating into, and its in-flight resource would land on a pool row that no
// longer exists.
func (s *Service) DeletePool(ctx context.Context, id, actor string) error {
	pool, err := s.GetPool(ctx, id)
	if err != nil {
		return err
	}
	nowMs := s.clock.Now().UnixMilli()
	return s.st.Tx(ctx, func(tx storage.TxStore) error {
		p, err := tx.Pools().Get(ctx, pool.ID)
		if err != nil {
			return err
		}
		var spec reconcile.PoolSpec
		if err := json.Unmarshal(p.Spec, &spec); err != nil {
			return invalid("pool spec: %v", err)
		}
		if spec.Replicas > 0 {
			return fmt.Errorf("%w: pool %s still wants %d replica(s); scale it to 0 first",
				storage.ErrConflict, p.Name, spec.Replicas)
		}
		members, err := tx.Resources().List(ctx, storage.ResourceFilter{PoolID: &p.ID})
		if err != nil {
			return err
		}
		if len(members) > 0 {
			return fmt.Errorf("%w: pool %s still has %d member(s); scale it to 0 and let them drain first",
				storage.ErrConflict, p.Name, len(members))
		}
		if err := tx.Pools().Delete(ctx, p.ID); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: actor, Type: "pool.deleted", PoolID: &p.ID, Outcome: "deleted",
		})
	})
}

func (s *Service) ReconcilePool(ctx context.Context, id string) error {
	pool, err := s.GetPool(ctx, id)
	if err != nil {
		return err
	}
	if pool.Paused {
		// Refusing is more honest than accepting a kick the paused
		// reconciler will discard.
		return fmt.Errorf("%w: pool %s is paused; resume it first", storage.ErrConflict, pool.Name)
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
