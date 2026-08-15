// Package metrics holds the cost-aware-leasing counters and histograms
// (docs/11 §18). Fleet gauges stay in app.FleetCollector (scrape-computed);
// these are event-driven and process-lifetime — Prometheus rate() handles
// the restart resets.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	ResourceReuse = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetplane_resource_reuse_total",
		Help: "Acquisitions bound to already-existing capacity instead of a new create",
	}, []string{"class"})

	ScaleUpAvoided = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetplane_scale_up_avoided_total",
		Help: "Queued acquisitions that bound to existing capacity instead of scaling up (docs/11 §7)",
	}, []string{"class"})

	AcquisitionQueueSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "fleetplane_acquisition_queue_seconds",
		Help:    "Time queued acquisitions waited before binding",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s .. ~34m
	})

	TerminationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fleetplane_resource_termination_seconds",
		Help:    "Delete operation duration, journal to provider-confirmed terminal",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10), // 1s .. ~8.5m
	}, []string{"provider"})

	BoundaryOverruns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetplane_billing_boundary_overruns_total",
		Help: "Deletes of billing-aware resources confirmed after the boundary they targeted",
	}, []string{"provider"})

	WindowMissed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetplane_billing_window_missed_total",
		Help: "Reclaim-eligible resources whose termination window passed unused (they wait a full extra increment)",
	}, []string{"provider"})

	PaidSeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetplane_resource_paid_seconds_total",
		Help: "Billed lifetime of terminated billing-aware resources (canonical paid(t), docs/11 §18)",
	}, []string{"provider", "kind"})

	UsefulSeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetplane_resource_useful_seconds_total",
		Help: "Leased (busy) time of terminated billing-aware resources — merged lease spans, not lease-seconds",
	}, []string{"provider", "kind"})

	PaidIdleSeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetplane_resource_paid_idle_seconds_total",
		Help: "Paid-but-idle time of terminated billing-aware resources (paid - useful, clamped at 0)",
	}, []string{"provider", "kind"})
)

// Register registers every cost metric on the given registry (boot calls
// this once; tests may pass their own registry).
func Register(reg prometheus.Registerer) {
	reg.MustRegister(ResourceReuse, ScaleUpAvoided, AcquisitionQueueSeconds,
		TerminationSeconds, BoundaryOverruns, WindowMissed,
		PaidSeconds, UsefulSeconds, PaidIdleSeconds)
}
