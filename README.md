# Fleetplane

Fleetplane is an **agentless, provider-driven control plane for infrastructure resources**, written in Go and shipped as a single static binary.

First-class use case: run the control plane on a very small always-on VM and create/destroy larger pre-imaged machines on demand (e.g. Hetzner Cloud CI runners) — reusing existing capacity before creating more, and reclaiming idle capacity by policy.

## Documentation

- Design docs: [`docs/`](docs/) (`00_README.md` … `10_FUTURE_DSL.md`)
- Architecture decision records: [`docs/adr/`](docs/adr/)

## Development

```sh
make gate    # build + vet + fmt + tidy + test(-race) + boundaries + lint
```

Every increment must end with `make gate` green. Kernel purity (orchestration code never imports provider SDKs) is enforced mechanically — see `.golangci.yml` (depguard) and `scripts/check-boundaries.sh`.

End-to-end tests against a real Hetzner project are opt-in: set `FLEETPLANE_E2E=1` and `HETZNER_TOKEN` (dedicated test project only; all test resources are labeled and swept).
