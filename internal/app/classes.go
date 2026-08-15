package app

// Class resolution and management (04 §2, dynamic classes): classes live in
// storage. Config classes are seeded at boot with source=config and stay
// config-authoritative (immutable via the API; pruned when removed from the
// file). API classes (source=api) are managed through /v1/classes. The
// resolver reads storage live, so template and policy changes apply on the
// next reconcile/satisfy pass without a restart.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/kinds"
	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

// Classes is the static map resolver (tests and embedded uses).
type Classes map[string]reconcile.Class

func (c Classes) Class(name string) (reconcile.Class, bool) {
	cls, ok := c[name]
	return cls, ok
}

// ClassRegistry is the storage-backed resolver used by the running server.
type ClassRegistry struct {
	st  storage.Store
	log *slog.Logger
}

func NewClassRegistry(st storage.Store, log *slog.Logger) *ClassRegistry {
	return &ClassRegistry{st: st, log: log}
}

// Class implements reconcile.ClassResolver.
func (r *ClassRegistry) Class(name string) (reconcile.Class, bool) {
	rec, err := r.st.Classes().Get(context.Background(), name)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			r.log.Error("class lookup", "class", name, "error", err)
		}
		return reconcile.Class{}, false
	}
	return toReconcileClass(rec), true
}

func toReconcileClass(rec *storage.ClassRecord) reconcile.Class {
	cls := reconcile.Class{Kind: rec.Kind, Provider: string(rec.Provider), Spec: rec.Spec}
	// ANY reclaim field constitutes a policy — a deleteAfter-only or
	// park-only class must not silently lose it (design finding).
	if (rec.ReclaimIdleAfterMs != nil && *rec.ReclaimIdleAfterMs > 0) || rec.ReclaimPark != "" ||
		(rec.ReclaimDeleteAfterMs != nil && *rec.ReclaimDeleteAfterMs > 0) {
		cls.Reclaim = &reconcile.ReclaimPolicy{Park: rec.ReclaimPark}
		if rec.ReclaimIdleAfterMs != nil {
			cls.Reclaim.IdleAfter = compute.Duration(time.Duration(*rec.ReclaimIdleAfterMs) * time.Millisecond)
		}
		if rec.ReclaimDeleteAfterMs != nil {
			cls.Reclaim.DeleteAfter = compute.Duration(time.Duration(*rec.ReclaimDeleteAfterMs) * time.Millisecond)
		}
	}
	if rec.QueueMaxWaitMs != nil && *rec.QueueMaxWaitMs > 0 {
		cls.QueueMaxWait = time.Duration(*rec.QueueMaxWaitMs) * time.Millisecond
	}
	return cls
}

// SeedConfigClasses makes the classes table reflect the config file's
// classes exactly (upsert current, prune stale config rows). API classes
// are untouched. Runs once at boot, before the reconciler starts.
func SeedConfigClasses(ctx context.Context, st storage.Store, classes map[string]reconcile.Class, nowMs int64) error {
	return st.Tx(ctx, func(tx storage.TxStore) error {
		existing, err := tx.Classes().ListBySource(ctx, "config")
		if err != nil {
			return err
		}
		for _, rec := range existing {
			if _, ok := classes[rec.Name]; !ok {
				if err := tx.Classes().Delete(ctx, rec.Name); err != nil {
					return err
				}
			}
		}
		for name, cls := range classes {
			rec := &storage.ClassRecord{
				Name: name, Kind: cls.Kind, Provider: storage.ProviderInstance(cls.Provider),
				Spec: cls.Spec, Source: "config", CreatedAt: nowMs, UpdatedAt: nowMs,
			}
			if r := cls.Reclaim; r != nil {
				if r.IdleAfter.Std() > 0 {
					ms := r.IdleAfter.Std().Milliseconds()
					rec.ReclaimIdleAfterMs = &ms
				}
				rec.ReclaimPark = r.Park
				if r.DeleteAfter.Std() > 0 {
					ms := r.DeleteAfter.Std().Milliseconds()
					rec.ReclaimDeleteAfterMs = &ms
				}
			}
			if cls.QueueMaxWait > 0 {
				ms := cls.QueueMaxWait.Milliseconds()
				rec.QueueMaxWaitMs = &ms
			}
			if err := tx.Classes().Upsert(ctx, rec); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- service CRUD ---

type UpsertClassCmd struct {
	Name         string
	Kind         string
	Provider     string
	Template     json.RawMessage
	ReclaimIdle  time.Duration
	ReclaimPark  string // "", "auto", "never"
	ReclaimDel   time.Duration
	QueueMaxWait time.Duration
	Actor        string
	// MustCreate: POST semantics — fail with ErrClassExists when present.
	MustCreate bool
}

var (
	ErrClassExists      = errors.New("class already exists")
	ErrClassConfigOwned = errors.New("class is defined in the config file; edit config.yaml and restart to change it")
)

// UpsertClass validates and writes an api-sourced class. The spec is
// validated against the kind registry HERE, at definition time — a bad
// template can never reach the provisioning path.
func (s *Service) UpsertClass(ctx context.Context, cmd UpsertClassCmd) (*storage.ClassRecord, error) {
	name := strings.TrimSpace(cmd.Name)
	if name == "" || len(name) > 63 || strings.ContainsAny(name, " \t\n/") {
		return nil, invalid("class name must be 1-63 characters with no whitespace or '/'")
	}
	if _, ok := kinds.Get(provider.ResourceKind(cmd.Kind)); !ok {
		return nil, invalid("unknown kind %q (registered: %v)", cmd.Kind, kinds.Names())
	}
	if err := kinds.Validate(provider.ResourceKind(cmd.Kind), cmd.Template); err != nil {
		return nil, invalid("template: %s", err.Error())
	}
	if _, ok := s.providers.Instance(storage.ProviderInstance(cmd.Provider)); !ok {
		return nil, invalid("unknown provider instance %q", cmd.Provider)
	}
	if cmd.ReclaimIdle < 0 || cmd.ReclaimDel < 0 || cmd.QueueMaxWait < 0 {
		return nil, invalid("reclaim and queue durations must be >= 0")
	}
	if p := cmd.ReclaimPark; p != "" && p != "auto" && p != "never" {
		return nil, invalid("reclaim.park must be auto or never, got %q", p)
	}
	if pt := s.pendingTimeoutMs; pt > 0 && cmd.QueueMaxWait.Milliseconds() > pt {
		return nil, invalid("scheduling.queue.maxWait exceeds acquire.pendingTimeout (%s)",
			time.Duration(pt)*time.Millisecond)
	}

	nowMs := s.clock.Now().UnixMilli()
	rec := &storage.ClassRecord{
		Name: name, Kind: cmd.Kind, Provider: storage.ProviderInstance(cmd.Provider),
		Spec: cmd.Template, Source: "api", CreatedAt: nowMs, UpdatedAt: nowMs,
	}
	if cmd.ReclaimIdle > 0 {
		ms := cmd.ReclaimIdle.Milliseconds()
		rec.ReclaimIdleAfterMs = &ms
	}
	rec.ReclaimPark = cmd.ReclaimPark
	if cmd.ReclaimDel > 0 {
		ms := cmd.ReclaimDel.Milliseconds()
		rec.ReclaimDeleteAfterMs = &ms
	}
	if cmd.QueueMaxWait > 0 {
		ms := cmd.QueueMaxWait.Milliseconds()
		rec.QueueMaxWaitMs = &ms
	}

	err := s.st.Tx(ctx, func(tx storage.TxStore) error {
		if cur, err := tx.Classes().Get(ctx, name); err == nil {
			if cur.Source == "config" {
				return ErrClassConfigOwned
			}
			if cmd.MustCreate {
				return ErrClassExists
			}
			rec.CreatedAt = cur.CreatedAt
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err := tx.Classes().Upsert(ctx, rec); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: cmd.Actor, Type: "class.upserted", Outcome: "ok",
			Details: json.RawMessage(fmt.Sprintf(`{"class":%q,"kind":%q,"provider":%q}`, name, cmd.Kind, cmd.Provider)),
		})
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Service) GetClass(ctx context.Context, name string) (*storage.ClassRecord, error) {
	return s.st.Classes().Get(ctx, name)
}

func (s *Service) ListClasses(ctx context.Context) ([]*storage.ClassRecord, error) {
	return s.st.Classes().List(ctx)
}

// DeleteClass removes an api-sourced class. Gates: config-owned classes
// are immutable here, and a class referenced by any pool cannot be deleted
// (the pool's creates would start failing). Resources already created from
// the class keep running; their name-keyed policies simply stop applying.
func (s *Service) DeleteClass(ctx context.Context, name, actor string) error {
	nowMs := s.clock.Now().UnixMilli()
	return s.st.Tx(ctx, func(tx storage.TxStore) error {
		rec, err := tx.Classes().Get(ctx, name)
		if err != nil {
			return err
		}
		if rec.Source == "config" {
			return ErrClassConfigOwned
		}
		pools, err := tx.Pools().List(ctx)
		if err != nil {
			return err
		}
		for _, p := range pools {
			var spec reconcile.PoolSpec
			if json.Unmarshal(p.Spec, &spec) == nil && spec.Class == name {
				return fmt.Errorf("%w: pool %q references class %q", storage.ErrConflict, p.Name, name)
			}
		}
		if err := tx.Classes().Delete(ctx, name); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: actor, Type: "class.deleted", Outcome: "ok",
			Details: json.RawMessage(fmt.Sprintf(`{"class":%q}`, name)),
		})
	})
}
