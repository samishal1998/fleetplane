package app

import "github.com/samimishal/fleetplane/internal/reconcile"

// Classes is the config-backed class resolver (04 §2) consumed by the
// reconciler and (at I8) the acquisition scheduler.
type Classes map[string]reconcile.Class

func (c Classes) Class(name string) (reconcile.Class, bool) {
	cls, ok := c[name]
	return cls, ok
}
