package app

// Prometheus surface (07 §7). Fleet gauges are computed on scrape via a
// custom Collector — no event-driven gauge drift.

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/samishal1998/fleetplane/internal/storage"
)

var (
	descResources = prometheus.NewDesc("fleetplane_resources",
		"Resources by provider, kind and phase (tombstoned excluded)",
		[]string{"provider", "kind", "phase"}, nil)
	descLeases = prometheus.NewDesc("fleetplane_leases_active",
		"Active leases", nil, nil)
	descOps = prometheus.NewDesc("fleetplane_operations",
		"Non-terminal operations by state (uncertain > 0 wants an operator)",
		[]string{"state"}, nil)
	descProviderHealth = prometheus.NewDesc("fleetplane_provider_health_state",
		"Provider health (1 = current state)", []string{"provider", "state"}, nil)
	descPools = prometheus.NewDesc("fleetplane_pools", "Configured pools", nil, nil)
)

// FleetCollector computes fleet gauges from storage + health on scrape.
type FleetCollector struct {
	st     storage.Store
	health *HealthTracker
}

func NewFleetCollector(st storage.Store, health *HealthTracker) *FleetCollector {
	return &FleetCollector{st: st, health: health}
}

func (c *FleetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descResources
	ch <- descLeases
	ch <- descOps
	ch <- descProviderHealth
	ch <- descPools
}

func (c *FleetCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if resources, err := c.st.Resources().List(ctx, storage.ResourceFilter{}); err == nil {
		type key struct{ provider, kind, phase string }
		counts := map[key]float64{}
		leases := 0.0
		for _, r := range resources {
			counts[key{string(r.Provider), r.Kind, string(r.Phase)}]++
			if n, err := c.st.Leases().CountActive(ctx, r.ID); err == nil {
				leases += float64(n)
			}
		}
		for k, v := range counts {
			ch <- prometheus.MustNewConstMetric(descResources, prometheus.GaugeValue, v, k.provider, k.kind, k.phase)
		}
		ch <- prometheus.MustNewConstMetric(descLeases, prometheus.GaugeValue, leases)
	}
	if ops, err := c.st.Operations().NonTerminal(ctx); err == nil {
		byState := map[string]float64{}
		for _, op := range ops {
			byState[string(op.State)]++
		}
		byState["uncertain"] += 0 // always exported: the alert metric (R23)
		for state, v := range byState {
			ch <- prometheus.MustNewConstMetric(descOps, prometheus.GaugeValue, v, state)
		}
	}
	if pools, err := c.st.Pools().List(ctx); err == nil {
		ch <- prometheus.MustNewConstMetric(descPools, prometheus.GaugeValue, float64(len(pools)))
	}
	if c.health != nil {
		for _, ph := range c.health.Snapshot() {
			ch <- prometheus.MustNewConstMetric(descProviderHealth, prometheus.GaugeValue, 1, ph.Instance, ph.State)
		}
	}
}
