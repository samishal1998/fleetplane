package billing

import (
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

var hourly = provider.BillingPolicy{BillingIncrement: time.Hour, TerminationBuffer: 5 * time.Minute}

func TestPaid(t *testing.T) {
	cases := []struct {
		name     string
		lifetime time.Duration
		p        provider.BillingPolicy
		want     time.Duration
	}{
		{"zero lifetime pays first increment", 0, hourly, time.Hour},
		{"partial hour rounds up", 10 * time.Minute, hourly, time.Hour},
		{"exact hour stays", time.Hour, hourly, time.Hour},
		{"just over rounds up", time.Hour + time.Millisecond, hourly, 2 * time.Hour},
		{"minimum swallows increments", 40 * time.Minute,
			provider.BillingPolicy{MinimumDuration: 90 * time.Minute, BillingIncrement: time.Hour}, 90 * time.Minute},
		{"past minimum uses increments", 100 * time.Minute,
			provider.BillingPolicy{MinimumDuration: 90 * time.Minute, BillingIncrement: time.Hour}, 2 * time.Hour},
		{"fine-grained is exact", 42 * time.Minute, provider.BillingPolicy{}, 42 * time.Minute},
		{"minimum only", 10 * time.Minute, provider.BillingPolicy{MinimumDuration: time.Hour}, time.Hour},
		{"minimum only, exceeded", 90 * time.Minute, provider.BillingPolicy{MinimumDuration: time.Hour}, 90 * time.Minute},
	}
	for _, c := range cases {
		if got := Paid(c.lifetime, c.p); got != c.want {
			t.Errorf("%s: Paid(%v) = %v, want %v", c.name, c.lifetime, got, c.want)
		}
	}
}

func TestNextBoundary(t *testing.T) {
	created := int64(1_000_000)
	ms := func(d time.Duration) int64 { return created + d.Milliseconds() }

	// hourly: boundaries every hour after creation
	if b, ok := NextBoundary(created, hourly, ms(10*time.Minute)); !ok || b != ms(time.Hour) {
		t.Fatalf("hourly at +10m: %d %v", b, ok)
	}
	if b, ok := NextBoundary(created, hourly, ms(61*time.Minute)); !ok || b != ms(2*time.Hour) {
		t.Fatalf("hourly at +61m: %d %v", b, ok)
	}
	// minimum 90m / increment 60m: paid() jumps at +60m (90 -> 120), then +120m
	p := provider.BillingPolicy{MinimumDuration: 90 * time.Minute, BillingIncrement: time.Hour}
	if b, ok := NextBoundary(created, p, ms(10*time.Minute)); !ok || b != ms(time.Hour) {
		t.Fatalf("min90/inc60 at +10m: got +%dm", (b-created)/60000)
	}
	// minimum 180m / increment 60m: first jump is at the minimum itself
	p = provider.BillingPolicy{MinimumDuration: 3 * time.Hour, BillingIncrement: time.Hour}
	if b, ok := NextBoundary(created, p, ms(10*time.Minute)); !ok || b != ms(3*time.Hour) {
		t.Fatalf("min180/inc60 at +10m: got +%dm", (b-created)/60000)
	}
	// minimum only: one boundary, then fine-grained
	p = provider.BillingPolicy{MinimumDuration: time.Hour}
	if b, ok := NextBoundary(created, p, ms(10*time.Minute)); !ok || b != ms(time.Hour) {
		t.Fatal("minimum-only boundary missing")
	}
	if _, ok := NextBoundary(created, p, ms(2*time.Hour)); ok {
		t.Fatal("minimum-only must be fine-grained past the minimum")
	}
	// fine-grained: never a boundary
	if _, ok := NextBoundary(created, provider.BillingPolicy{}, ms(time.Hour)); ok {
		t.Fatal("fine-grained must have no boundary")
	}
}

func TestEffectiveBufferFloor(t *testing.T) {
	// Zero policy buffer must still yield a window a 15s sweep can hit.
	if got := EffectiveBuffer(provider.BillingPolicy{BillingIncrement: time.Hour}, 15*time.Second); got != 35*time.Second {
		t.Fatalf("floor = %v", got)
	}
	if got := EffectiveBuffer(hourly, 15*time.Second); got != 5*time.Minute {
		t.Fatalf("explicit buffer overridden: %v", got)
	}
}

func TestWindowDecide(t *testing.T) {
	created := int64(0)
	buffer := 6 * time.Minute
	w, ok := TerminationWindow(created, hourly, 10*60_000, buffer, 0)
	if !ok {
		t.Fatal("window expected")
	}
	hour := time.Hour.Milliseconds()
	if w.BoundaryMs != hour || w.StartMs != hour-buffer.Milliseconds() || w.CutoffMs != hour-3*60_000 {
		t.Fatalf("window = %+v", w)
	}
	if d := w.Decide(hour - 10*60_000); d != Keep {
		t.Fatalf("before window: %v", d)
	}
	if d := w.Decide(hour - 5*60_000); d != Terminate {
		t.Fatalf("inside window: %v", d)
	}
	if d := w.Decide(hour - 60_000); d != Crossed {
		t.Fatalf("past cutoff must be Crossed (intentional crossing, docs/11 §14): %v", d)
	}
	// Once crossed, the next evaluation targets the NEXT boundary.
	w2, _ := TerminationWindow(created, hourly, hour+60_000, buffer, 0)
	if w2.BoundaryMs != 2*hour {
		t.Fatalf("next boundary = %d", w2.BoundaryMs)
	}
}

func TestAdaptiveBuffer(t *testing.T) {
	obs := make([]time.Duration, 100)
	for i := range obs {
		obs[i] = time.Duration(i+1) * time.Second // p95 = 96s
	}
	got := AdaptiveBuffer(obs, 30*time.Second, time.Minute, 30*time.Minute)
	if got != 96*time.Second+time.Minute {
		t.Fatalf("adaptive = %v", got)
	}
	if got := AdaptiveBuffer(nil, 30*time.Second, time.Minute, 0); got != 30*time.Second {
		t.Fatalf("floor without samples = %v", got)
	}
	// Cap: a provider incident must not blow the buffer past increment/2.
	if got := AdaptiveBuffer([]time.Duration{2 * time.Hour}, 30*time.Second, time.Minute, 30*time.Minute); got != 30*time.Minute {
		t.Fatalf("cap = %v", got)
	}
}

func TestUsefulMillis(t *testing.T) {
	// Overlapping shared leases count once (busy time, not lease-seconds).
	spans := []Interval{{0, 10_000}, {5_000, 15_000}, {20_000, 0}} // open-ended
	if got := UsefulMillis(spans, 25_000); got != 20_000 {
		t.Fatalf("useful = %d, want 20000", got)
	}
	if got := UsefulMillis(nil, 1000); got != 0 {
		t.Fatalf("empty = %d", got)
	}
}
