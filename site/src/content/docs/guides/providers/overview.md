---
title: "Providers overview"
description: "Drivers, provider instances, health, and which driver supports billing windows and parking."
---

A **provider** is Fleetplane's connection to one external infrastructure authority —
Hetzner Cloud, DigitalOcean, AWS, GCP, or anything else that can create and destroy
resources. A **driver** is the code (`hetzner`, `digitalocean`, `aws`, `gcp`,
`fake`); a **provider instance**
is one configured use of a driver, named by you in the config file. You can run
several instances of the same driver (say, two Hetzner projects) side by side.

```yaml
providers:
  hetzner-main:            # instance name — referenced by classes and pools
    driver: hetzner        # driver name — must be compiled into the binary
    settings:              # raw driver config; credentials are secret:// refs
      token: secret://env/HETZNER_TOKEN
      location: fsn1
```

Six drivers ship in the default binary (see
[`cmd/fleetplane/modules.go`](https://github.com/samishal1998/fleetplane/blob/main/cmd/fleetplane/modules.go)):

| Driver | Kinds | Notes |
|---|---|---|
| `hetzner` | `compute.machine` | Hetzner Cloud servers via hcloud-go v2 |
| `digitalocean` | `compute.machine` | DigitalOcean droplets via godo |
| `aws` | `compute.machine` | Amazon EC2 instances via aws-sdk-go-v2 |
| `gcp` | `compute.machine` | Google Compute Engine instances via compute/v1 |
| `docker` | `compute.machine` | Containers on a local/remote Docker Engine — real provider, no credentials |
| `fake` | `compute.machine`, `storage.volume` | Deterministic in-memory provider for tests |

Check instance health with `fleetplane providers` or the Providers view of the
[web dashboard](/fleetplane/guides/dashboard/) at `http://<server.addr>/ui/`. Health walks
`unknown → healthy → degraded → unavailable` (checked every 30s; degraded after 3
consecutive failures, unavailable after 10) and never affects `/health/ready` —
a cloud outage must not take the control plane out of rotation (design docs,
[07 §8](https://github.com/samishal1998/fleetplane/blob/main/docs/07_SECURITY_AND_OPERATIONS.md)).

For the config file as a whole, see the [configuration guide](/fleetplane/guides/configuration/).

## Drivers at a glance

| Driver | Billing policy (default) | Parking |
|---|---|---|
| [`hetzner`](/fleetplane/guides/providers/hetzner/) | Hourly increment + `5m` termination buffer | No — powered-off servers bill at full price |
| [`digitalocean`](/fleetplane/guides/providers/digitalocean/) | Hourly increment + `5m` termination buffer | No — powered-off droplets bill at full price |
| [`aws`](/fleetplane/guides/providers/aws/) | `60s` minimum, then per-second | Yes (EBS-backed instances) |
| [`gcp`](/fleetplane/guides/providers/gcp/) | `60s` minimum, then per-second | Yes |
| [`docker`](/fleetplane/guides/providers/docker/) | — | Yes (stopped containers) |
| [`fake`](/fleetplane/guides/providers/fake/) | Whatever its settings declare | Opt-in via `settings.park` |

Credentials are always `secret://` references — see [configuration → credentials](/fleetplane/guides/configuration/#credentials-secret-references). To add a driver of your own, see [writing a provider](/fleetplane/developers/provider-sdk/).
