package compute

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

// ProbeOnce attempts the readiness probe once against the first usable
// address. The retry loop (initial delay, period, budget, threshold) is
// owned by the operation engine (plan R16); this is the single attempt.
func ProbeOnce(ctx context.Context, addrs []provider.Address, spec *ReadinessSpec) error {
	if spec == nil || spec.TCP == nil {
		return nil // no probe configured => provider-level readiness suffices
	}
	addr := pickAddress(addrs)
	if addr == "" {
		return fmt.Errorf("readiness probe: resource has no usable address")
	}
	timeout := spec.TCP.Timeout.Std()
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr, strconv.Itoa(spec.TCP.Port)))
	if err != nil {
		return fmt.Errorf("readiness probe: tcp %s:%d: %w", addr, spec.TCP.Port, err)
	}
	_ = conn.Close()
	return nil
}

// pickAddress prefers public IPv4, then public IPv6, then private.
func pickAddress(addrs []provider.Address) string {
	order := []string{"public-v4", "public-v6", "private"}
	for _, want := range order {
		for _, a := range addrs {
			if a.Network == want && a.Addr != "" {
				return a.Addr
			}
		}
	}
	for _, a := range addrs {
		if a.Addr != "" {
			return a.Addr
		}
	}
	return ""
}
