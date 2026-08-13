// Package sdk holds the small shared seams between the kernel, providers and
// test tooling that belong to no larger contract package.
package sdk

import "time"

// Clock is the time seam (ADR reconciliation R13): every kernel component
// with TTL/idle/cooldown/backoff logic takes a Clock so tests control time
// deterministically. The production implementation is Real.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Real is the wall-clock Clock.
type Real struct{}

func (Real) Now() time.Time                         { return time.Now() }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }
