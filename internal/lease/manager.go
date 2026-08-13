// Package lease manages lease lifecycle: release, TTL expiry sweeps, and
// the allocated→ready transition when the last active lease ends. A leased
// resource is never reclaimed (invariant 3) — this package is what makes
// "leased" end.
package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/samimishal/fleetplane/internal/phase"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/pkg/sdk"
)

// PoolKicker level-triggers the reconciler when capacity frees up.
type PoolKicker interface {
	Kick(pool storage.PoolID)
	KickAll()
}

type Manager struct {
	st    storage.Store
	clock sdk.Clock
	log   *slog.Logger
	pools PoolKicker
}

func New(st storage.Store, clock sdk.Clock, log *slog.Logger, pools PoolKicker) *Manager {
	return &Manager{st: st, clock: clock, log: log, pools: pools}
}

// Release ends a lease (caller-initiated). The acquisition, when bound to
// this lease, moves to released.
func (m *Manager) Release(ctx context.Context, leaseID storage.LeaseID) error {
	nowMs := m.clock.Now().UnixMilli()
	var poolID *storage.PoolID
	err := m.st.Tx(ctx, func(tx storage.TxStore) error {
		l, err := tx.Leases().Get(ctx, leaseID)
		if err != nil {
			return err
		}
		if l.State != storage.LeaseActive {
			return fmt.Errorf("%w: lease %s is %s", storage.ErrConflict, leaseID, l.State)
		}
		if err := tx.Leases().Transition(ctx, leaseID, storage.LeaseActive, storage.LeaseReleased, nowMs); err != nil {
			return err
		}
		acq, err := tx.Acquisitions().Get(ctx, l.AcquisitionID)
		if err == nil && acq.State == storage.AcqBound {
			_ = tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqBound, storage.AcqReleased, nowMs)
		}
		pid, err := m.endLeaseLocked(ctx, tx, l, nowMs)
		if err != nil {
			return err
		}
		poolID = pid
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Type: "lease.released", ResourceID: &l.ResourceID,
			Details: json.RawMessage(fmt.Sprintf(`{"leaseId":%q,"acquisitionId":%q}`, l.ID, l.AcquisitionID)),
		})
	})
	if err != nil {
		return err
	}
	m.kick(poolID)
	return nil
}

// ReleaseAcquisition releases via the acquisition handle (DELETE
// /v1/acquisitions/{id}, 04 §3).
func (m *Manager) ReleaseAcquisition(ctx context.Context, acqID storage.AcquisitionID) error {
	acq, err := m.st.Acquisitions().Get(ctx, acqID)
	if err != nil {
		return err
	}
	switch acq.State {
	case storage.AcqBound:
		if acq.LeaseID == nil {
			return fmt.Errorf("%w: bound acquisition without lease", storage.ErrConflict)
		}
		return m.Release(ctx, *acq.LeaseID)
	case storage.AcqPending, storage.AcqProvisioning:
		// Withdraw before satisfaction: the pre-bound resource (if any)
		// stays as pool/idle capacity and is reclaimed by policy.
		nowMs := m.clock.Now().UnixMilli()
		return m.st.Tx(ctx, func(tx storage.TxStore) error {
			return tx.Acquisitions().Transition(ctx, acqID, acq.State, storage.AcqReleased, nowMs)
		})
	default:
		return fmt.Errorf("%w: acquisition %s is %s", storage.ErrConflict, acqID, acq.State)
	}
}

// SweepExpired expires overdue leases (TTL, 04 §4) and returns how many.
func (m *Manager) SweepExpired(ctx context.Context) (int, error) {
	nowMs := m.clock.Now().UnixMilli()
	due, err := m.st.Leases().Expiring(ctx, nowMs, 100)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range due {
		var poolID *storage.PoolID
		err := m.st.Tx(ctx, func(tx storage.TxStore) error {
			if err := tx.Leases().Transition(ctx, l.ID, storage.LeaseActive, storage.LeaseExpired, nowMs); err != nil {
				return err
			}
			acq, err := tx.Acquisitions().Get(ctx, l.AcquisitionID)
			if err == nil && acq.State == storage.AcqBound {
				_ = tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqBound, storage.AcqExpired, nowMs)
			}
			pid, err := m.endLeaseLocked(ctx, tx, l, nowMs)
			if err != nil {
				return err
			}
			poolID = pid
			return tx.Events().Append(ctx, &storage.Event{
				TS: nowMs, Type: "lease.expired", ResourceID: &l.ResourceID,
				Details: json.RawMessage(fmt.Sprintf(`{"leaseId":%q}`, l.ID)),
			})
		})
		if err != nil {
			m.log.Error("lease expiry", "lease_id", l.ID, "error", err)
			continue
		}
		n++
		m.kick(poolID)
	}
	return n, nil
}

// endLeaseLocked stamps idle-tracking and returns the resource's pool; when
// the last active lease ended, allocated → ready.
func (m *Manager) endLeaseLocked(ctx context.Context, tx storage.TxStore, l *storage.Lease, nowMs int64) (*storage.PoolID, error) {
	if err := tx.Resources().SetLastLeaseEnded(ctx, l.ResourceID, nowMs); err != nil {
		return nil, err
	}
	res, err := tx.Resources().Get(ctx, l.ResourceID)
	if err != nil {
		return nil, err
	}
	active, err := tx.Leases().CountActive(ctx, l.ResourceID)
	if err != nil {
		return nil, err
	}
	if active == 0 && res.Phase == phase.Allocated {
		if err := tx.Resources().CASPhase(ctx, l.ResourceID, phase.Allocated, phase.Ready, nowMs); err != nil {
			return nil, err
		}
	}
	return res.PoolID, nil
}

func (m *Manager) kick(pool *storage.PoolID) {
	if m.pools == nil {
		return
	}
	if pool != nil {
		m.pools.Kick(*pool)
	} else {
		m.pools.KickAll() // poolless: the idle sweeper reclaims by class policy
	}
}

// Run sweeps on a ticker until ctx is done.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.clock.After(interval):
			if _, err := m.SweepExpired(ctx); err != nil && ctx.Err() == nil {
				m.log.Error("lease sweep", "error", err)
			}
		}
	}
}
