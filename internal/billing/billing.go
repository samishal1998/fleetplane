// Package billing is the pure math of docs/11: one canonical paid(t)
// function, its discontinuities (billing boundaries), the termination
// window with a point-of-no-return cutoff, and the paid/useful accounting.
// No storage, no providers, no clocks — every caller supplies now.
package billing

import (
	"sort"
	"time"

	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

// Paid returns the total billed duration for a resource that lived for
// `lifetime` under the policy (docs/11 §10; canonical — the boundary
// math and the §18 accounting both derive from this one function):
//
//	increment > 0: max(minimum, ceil(lifetime/increment) * increment)
//	increment = 0: max(minimum, lifetime)
func Paid(lifetime time.Duration, p provider.BillingPolicy) time.Duration {
	if lifetime < 0 {
		lifetime = 0
	}
	billed := lifetime
	if p.BillingIncrement > 0 {
		n := lifetime / p.BillingIncrement
		if lifetime == 0 || lifetime%p.BillingIncrement != 0 {
			n++
		}
		if n == 0 {
			n = 1
		}
		billed = n * p.BillingIncrement
	}
	if billed < p.MinimumDuration {
		billed = p.MinimumDuration
	}
	return billed
}

// NextBoundary returns the next instant strictly after now at which Paid
// increases (the next economically significant boundary, docs/11 §10).
// ok=false means the policy is fine-grained past this point: no boundary,
// ordinary idle policies apply.
func NextBoundary(createdAtMs int64, p provider.BillingPolicy, nowMs int64) (boundaryMs int64, ok bool) {
	elapsed := time.Duration(nowMs-createdAtMs) * time.Millisecond
	if elapsed < 0 {
		elapsed = 0
	}
	if p.BillingIncrement > 0 {
		// Boundaries are the discontinuities of Paid: t = k*increment where
		// Paid actually jumps (a large minimum swallows early increments).
		for k := elapsed/p.BillingIncrement + 1; ; k++ {
			t := k * p.BillingIncrement
			if Paid(t+time.Millisecond, p) > Paid(t-time.Millisecond, p) {
				return createdAtMs + t.Milliseconds(), true
			}
			// Paid is flat only while under the minimum; bounded loop.
			if t > p.MinimumDuration+p.BillingIncrement {
				return createdAtMs + t.Milliseconds(), true
			}
		}
	}
	if p.MinimumDuration > 0 && elapsed < p.MinimumDuration {
		// One boundary at the end of the minimum; fine-grained after it.
		return createdAtMs + p.MinimumDuration.Milliseconds(), true
	}
	return 0, false
}

// EffectiveBuffer applies the kernel floor to a policy's termination
// buffer: a zero or tiny buffer would make the termination window
// unhittable by a periodic sweep, silently stranding resources forever
// (design verification finding). The floor is 2×sweepInterval + 5s.
func EffectiveBuffer(p provider.BillingPolicy, sweepInterval time.Duration) time.Duration {
	floor := 2*sweepInterval + 5*time.Second
	if p.TerminationBuffer > floor {
		return p.TerminationBuffer
	}
	return floor
}

// Window is the interval in which a cost-based delete may be dispatched
// for the boundary: [Start, Cutoff]. Past Cutoff the next increment is
// considered inescapable — the economically correct move is to KEEP the
// resource and target the next boundary (docs/11 §14: crossing becomes an
// explicit decision, never an accident).
type Window struct {
	BoundaryMs int64
	StartMs    int64 // boundary - buffer
	CutoffMs   int64 // boundary - cutoff (point of no return)
}

// TerminationWindow computes the delete window before the next boundary.
// cutoff defaults to buffer/2 when the caller has no observed deletion
// p95. ok=false: fine-grained policy, no window semantics.
func TerminationWindow(createdAtMs int64, p provider.BillingPolicy, nowMs int64, buffer, cutoff time.Duration) (Window, bool) {
	b, ok := NextBoundary(createdAtMs, p, nowMs)
	if !ok {
		return Window{}, false
	}
	if cutoff <= 0 || cutoff > buffer {
		cutoff = buffer / 2
	}
	return Window{
		BoundaryMs: b,
		StartMs:    b - buffer.Milliseconds(),
		CutoffMs:   b - cutoff.Milliseconds(),
	}, true
}

// Decision for an idle, reclaim-eligible, billing-aware resource.
type Decision int

const (
	Keep      Decision = iota // before the window: paid capacity, keep it available
	Terminate                 // inside [start, cutoff]: dispatch the delete now
	Crossed                   // past cutoff: this boundary is lost — keep, target the next
)

// Decide places now against the termination window.
func (w Window) Decide(nowMs int64) Decision {
	switch {
	case nowMs < w.StartMs:
		return Keep
	case nowMs <= w.CutoffMs:
		return Terminate
	default:
		return Crossed
	}
}

// AdaptiveBuffer derives a termination buffer from observed provider
// deletion durations (docs/11 §12): p95 + margin, never below floor, and
// capped at cap (increment/2 in practice — past that, window timing is
// meaningless and the caller should log it).
func AdaptiveBuffer(observed []time.Duration, floor, margin, cap time.Duration) time.Duration {
	buf := floor
	if len(observed) > 0 {
		s := append([]time.Duration(nil), observed...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		p95 := s[(len(s)*95)/100]
		if p95+margin > buf {
			buf = p95 + margin
		}
	}
	if cap > 0 && buf > cap {
		return cap
	}
	return buf
}

// Interval is a [start, end] span in unix millis (end 0 = still open).
type Interval struct{ StartMs, EndMs int64 }

// UsefulMillis merges overlapping lease intervals and returns the total
// covered time — resource-busy seconds, not lease-seconds, so overlapping
// shared leases are not double-counted (§18 accounting).
func UsefulMillis(spans []Interval, nowMs int64) int64 {
	if len(spans) == 0 {
		return 0
	}
	s := append([]Interval(nil), spans...)
	for i := range s {
		if s[i].EndMs == 0 || s[i].EndMs > nowMs {
			s[i].EndMs = nowMs
		}
	}
	sort.Slice(s, func(i, j int) bool { return s[i].StartMs < s[j].StartMs })
	total, curStart, curEnd := int64(0), s[0].StartMs, s[0].EndMs
	for _, iv := range s[1:] {
		if iv.StartMs > curEnd {
			total += curEnd - curStart
			curStart, curEnd = iv.StartMs, iv.EndMs
			continue
		}
		if iv.EndMs > curEnd {
			curEnd = iv.EndMs
		}
	}
	total += curEnd - curStart
	if total < 0 {
		total = 0
	}
	return total
}
