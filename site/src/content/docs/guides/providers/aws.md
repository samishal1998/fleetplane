---
title: "AWS"
description: "Run Fleetplane machines on Amazon EC2: IAM policy, credentials chain, AMI selectors, billing and parking."
---

This page covers the `aws` driver: a walkthrough from credentials to first machine, the permissions it needs, and the full settings reference. For how providers fit together, see the [providers overview](/fleetplane/guides/providers/overview/).

## Walkthrough

1. **Create IAM credentials.** Create a dedicated IAM user (or role) with the minimal EC2 policy from [credentials and permissions](#credentials-and-permissions), then create an access key for it. Alternatively, skip static keys entirely: when Fleetplane itself runs on AWS, attach the policy to the instance's role and omit `accessKeyId`/`secretAccessKey` — the driver falls back to the SDK's ambient credential chain (environment, shared config/credentials files, IMDS/IRSA roles), and no keys go in the config at all.

2. **Export the key pair** where the server runs (or put it in the systemd `EnvironmentFile`):

   ```bash
   export AWS_ACCESS_KEY_ID=<access key id>
   export AWS_SECRET_ACCESS_KEY=<secret access key>
   ```

3. **Configure:**

   ```yaml
   providers:
     aws-main:
       driver: aws
       settings:
         region: eu-central-1                              # required
         accessKeyId: secret://env/AWS_ACCESS_KEY_ID       # omit BOTH keys for the ambient chain
         secretAccessKey: secret://env/AWS_SECRET_ACCESS_KEY
         # sessionToken: secret://env/AWS_SESSION_TOKEN    # only with temporary credentials
         # subnetId: subnet-0abc123                        # optional placement
         # securityGroupIds: [sg-0abc123]                  # optional
         # keyName: my-keypair                             # optional EC2 key pair
         # instanceProfile: my-profile                     # optional; needs iam:PassRole (see below)

   classes:
     ci-aws:
       kind: compute.machine
       provider: aws-main
       spec:
         serverType: t3.medium
         image: "id:ami-0abc1234567890def"
   ```

   `accessKeyId` and `secretAccessKey` are all-or-nothing — setting only one is a boot error, as is a `sessionToken` without the pair. `spec.location` is an availability zone (e.g. `eu-central-1a`). Optional pacing settings mirror the other drivers: `endpoint`, `rps`, `burst`, `maxConcurrent`.

4. **Image selector syntax:**

   | Form | Example | Selects |
   |---|---|---|
   | `id:<ami>` | `id:ami-0abc1234567890def` | An AMI by ID |
   | `name:<pattern>` | `name:ubuntu/images/hvm-ssd-gp3/*24.04*` | Newest available self- or Amazon-owned AMI matching the name pattern |
   | `snapshot:<k=v>` | `snapshot:ci-runner=v12` | Newest available self-owned AMI with that tag |

   `snapshot:` selects your own AMIs by tag equality — tag the AMIs your image pipeline produces (e.g. `ci-runner=v12`) and select by tag. When several match, the newest wins. A selector matching nothing is a fail-fast config error, not a retry.

5. **Try it:**

   ```bash
   fleetplane acquire --class ci-aws --cpu 2 --ttl 90m
   fleetplane watch acq_01J...
   fleetplane resources
   ```

AWS tag keys permit dots and slashes, so the `fleetplane.io/*` identity labels land on instances as native EC2 tags, verbatim. Leave them in place — discovery and crash recovery depend on them.

## Credentials and permissions

Create an **IAM user or role** with this minimal policy — everything the driver calls, nothing more:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "FleetplaneEC2",
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:TerminateInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeImages",
        "ec2:DescribeInstanceTypes",
        "ec2:DescribeRegions",
        "ec2:CreateTags"
      ],
      "Resource": "*"
    }
  ]
}
```

`ec2:DescribeRegions` backs the provider health check; `ec2:CreateTags` is required because every create tags the instance with the `fleetplane.io/*` identity labels. If you want to tighten `ec2:CreateTags` so the credential can only tag at instance creation (never re-tag existing resources), split it into its own statement with the condition `"StringEquals": {"ec2:CreateAction": "RunInstances"}` — kept simple by default above.

Add `iam:PassRole` **only** when `settings.instanceProfile` is configured — launching an instance with an instance profile passes its role:

```json
{
  "Sid": "FleetplanePassRole",
  "Effect": "Allow",
  "Action": "iam:PassRole",
  "Resource": "arn:aws:iam::<account-id>:role/<instance-profile-role>"
}
```

Wire the key pair through `secret://env`:

```yaml
settings:
  region: eu-central-1
  accessKeyId: secret://env/AWS_ACCESS_KEY_ID
  secretAccessKey: secret://env/AWS_SECRET_ACCESS_KEY
```

**No-keys alternative:** when Fleetplane itself runs on AWS, attach the policy to the instance's IAM role and omit `accessKeyId`/`secretAccessKey` entirely — the driver uses the SDK's ambient credential chain (environment variables, shared config/credentials files, IMDS/IRSA roles) and no secret ever appears in the config.

## Settings

```yaml
providers:
  aws-main:
    driver: aws
    settings:
      region: eu-central-1                              # required
      accessKeyId: secret://env/AWS_ACCESS_KEY_ID       # optional pair; omit both
      secretAccessKey: secret://env/AWS_SECRET_ACCESS_KEY   # for the ambient chain
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `region` | yes | — | AWS region (e.g. `eu-central-1`) |
| `accessKeyId` | no | ambient chain | Access key ID as a `secret://` reference; all-or-nothing with `secretAccessKey` |
| `secretAccessKey` | no | ambient chain | Secret access key as a `secret://` reference |
| `sessionToken` | no | none | Session token for temporary credentials; requires the static pair |
| `endpoint` | no | public AWS API | API base URL override (used by tests) |
| `subnetId` | no | account default | Subnet for created instances |
| `securityGroupIds` | no | account default | Security group IDs for created instances |
| `keyName` | no | none | EC2 key pair name for created instances |
| `instanceProfile` | no | none | IAM instance profile **name** attached to created instances (needs `iam:PassRole`) |
| `rps` | no | `5` | Sustained request rate toward the EC2 API |
| `burst` | no | `10` | Token-bucket burst size |
| `maxConcurrent` | no | `5` | Max in-flight API calls |

Setting only one of `accessKeyId`/`secretAccessKey` is a boot error
(`aws: accessKeyId and secretAccessKey are all-or-nothing`), as is a
`sessionToken` without the pair. When the pair is **omitted entirely**, the
driver uses the SDK's ambient credential chain — environment variables, shared
config/credentials files, IMDS/IRSA instance roles — so a Fleetplane host
running on AWS needs no keys in its config at all. The minimal IAM policy for
either route is in the
[credentials and permissions](#credentials-and-permissions).
SDK retries are capped at one attempt — the operation engine is the only retry
authority ([ADR-014](/fleetplane/developers/adr/adr-014-retries/)) — and the shared pacer shapes
request rate (`rps`/`burst`/`maxConcurrent`).

## Instance types and images

```yaml
classes:
  ci-aws:
    kind: compute.machine
    provider: aws-main
    spec:
      serverType: t3.medium
      image: "snapshot:ci-runner=v12"
      location: eu-central-1a        # availability zone (optional)
```

`serverType` is an EC2 instance type name, passed through as-is. Reported
capacity comes from `DescribeInstanceTypes` (cached per type): `cpu` = default
vCPUs, `memoryMiB` = memory in MiB. `spec.location`, when set, is the
availability zone.

`image` takes one of three forms:

| Form | Example | Resolution |
|---|---|---|
| `id:<ami>` | `id:ami-0abc1234567890def` | Exact AMI ID |
| `name:<pattern>` | `name:ubuntu/images/hvm-ssd-gp3/*24.04*` | Available self- and Amazon-owned AMIs by name pattern; **newest wins** |
| `snapshot:<k=v>` | `snapshot:ci-runner=v12` | Available self-owned AMIs by tag equality; **newest wins** |

`snapshot:` is the image-pipeline form: tag the AMIs you build
(`ci-runner=v12`) and select by tag; the most recently created match is used.
A selector that matches nothing is a configuration error: the create fails
fast with an `invalid` error (`no image matches ...`) instead of retrying.

## Billing

The driver declares EC2's per-second billing with a **60-second minimum** for
`compute.machine` — a minimum duration, no increment
([cost-aware leasing](/fleetplane/guides/concepts/#cost-aware-leasing-billing-windows)). With
no increment there are no billing-boundary windows to schedule deletes
around; the minimum only means a machine deleted within its first minute was
billed for the full minute. Override or disable per kind via
`providers.<name>.billing`
([configuration → billing](/fleetplane/guides/configuration/#billing-providersnamebilling)).

## Parking

The driver implements the optional `ParkAware` capability for
`compute.machine` ([concepts → parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier)):
EBS-backed instances stop and resume, and a stopped instance bills only its
EBS volumes and Elastic IPs. The declared `StartEstimate` is 30s — EC2
starts typically land in tens of seconds.

Two caveats the driver surfaces rather than hides:

- **Spot and instance-store-backed instances cannot stop.** EC2 rejects the
  call with `UnsupportedOperation`, which the driver maps to an `invalid`
  error — the kernel cleanly reverts `parking → ready` and falls back to
  delete/create semantics for that machine.
- **The public IP changes.** EC2 releases the ephemeral public IPv4 at stop
  and usually assigns a new one at start (only Elastic IPs are stable), so
  Fleetplane re-observes addresses and re-runs the readiness probe after
  every start — never reuse pre-stop addresses.

## Identity labels

AWS tag keys permit dots and slashes, so the `fleetplane.io/*` identity
labels ride as **native EC2 instance tags, verbatim — no codec**
([ADR-013](/fleetplane/developers/adr/adr-013-registration-labels/)). Extra labels from
`spec.labels` are merged in (plus a `Name` tag from the desired name); on a
key collision the reserved labels win. Discovery filters server-side on the
ownership tags; create-dedup finds the op tag, and the op ID additionally
rides as EC2's native `ClientToken` idempotency token. Don't remove
`fleetplane.io/*` tags — ownership tracking, discovery, and crash recovery
depend on them.

## Instance state mapping

| EC2 state | Fleetplane observed phase |
|---|---|
| `pending` | `pending` |
| `running` | `running` |
| `stopping`, `stopped` | `stopped` |
| `shutting-down` | `deleting` |
| `terminated` | `gone` |
| anything else | `unknown` |

Terminated instances stay visible on EC2 for up to an hour; `Discover` skips
them (they are artifacts, not resources) while `Get` reports them as `gone`.
The full native instance object is preserved in `status.extensions`.
