---
title: "Quickstart"
description: "Run the whole loop locally with the built-in fake provider: serve, acquire, reuse, release, pools."
---

No cloud account needed — the built-in `fake` provider behaves like a real one, including asynchronous creates.

Install a prebuilt binary (Linux/macOS; the script is [`install.sh`](https://github.com/samishal1998/fleetplane/blob/main/install.sh) in this repo):

```bash
curl -fsSL https://raw.githubusercontent.com/samishal1998/fleetplane/main/install.sh | sh
```

Or build from source (requires Go 1.26+):

```bash
git clone https://github.com/samishal1998/fleetplane
cd fleetplane
go build -o fleetplane ./cmd/fleetplane
```

Write a minimal `config.yaml`:

```yaml
server:
  addr: ":8080"

storage:
  path: ./fleetplane.db

providers:
  demo:
    driver: fake
    settings:
      createSteps: 2

classes:
  demo-small:
    kind: compute.machine
    provider: demo
    spec:
      serverType: cpx31
      image: "snapshot:demo=v1"
```

Start the control plane:

```bash
./fleetplane serve --config config.yaml
```

In another shell, acquire capacity — a machine is created and bound:

```bash
./fleetplane acquire --class demo-small --cpu 1 --ttl 30m
# acq_01J...  -> res_01J...
# state: provisioning

./fleetplane watch acq_01J...     # polls until bound
./fleetplane resources            # ID  KIND  PROVIDER  PHASE  EXTERNAL  NAME
```

Acquire again and the **same** machine is reused; release when done:

```bash
./fleetplane release acq_01J...
```

Or declare a pool and let the reconciler hold it at size:

```bash
./fleetplane apply -f - <<'EOF'
apiVersion: fleetplane.io/v1alpha1
kind: Pool
metadata:
  name: demo-pool
spec:
  class: demo-small
  replicas: 2
EOF

./fleetplane pools
```

Open the dashboard at <http://127.0.0.1:8080/ui/> to see the fleet live.

For the full self-verifying tour (reuse, idle reclaim, and crash recovery under `kill -9`), run:

```bash
./examples/demo.sh
```

To go from here to a real cloud, follow the **[setup guide](/fleetplane/guides/installation/)**.

## Verify

Health endpoints are served on both listeners:

```bash
curl -s http://127.0.0.1:8080/health/live    # {"status":"ok"} — always, once the process is up
curl -s http://127.0.0.1:8080/health/ready   # {"status":"ok"} — after storage ping + journal recovery
```

Readiness reflects storage and journal recovery only; provider outages never make Fleetplane unready.

Metrics (ops listener):

```bash
curl -s http://127.0.0.1:9090/metrics | grep '^fleetplane_'
# fleetplane_resources{provider,kind,phase}
# fleetplane_leases_active
# fleetplane_operations{state}        <- alert on state="uncertain" > 0
# fleetplane_provider_health_state{provider,state}
# fleetplane_pools
```

Provider health from the CLI:

```bash
fleetplane providers
# INSTANCE      DRIVER   STATE    FAILURES  LAST ERROR
# hetzner-main  hetzner  healthy  0
```

## Next steps

- Learn the model behind what you just did: [concepts](/fleetplane/guides/concepts/).
- Keep capacity warm with [pools](/fleetplane/guides/pools/), or lease it on demand: [acquiring capacity](/fleetplane/guides/acquiring/).
- Move to a real cloud: [installation](/fleetplane/guides/installation/) and the [providers](/fleetplane/guides/providers/overview/).
