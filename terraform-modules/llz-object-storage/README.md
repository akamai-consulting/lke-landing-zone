# `object-storage`

Linode Object Storage buckets for a platform's container registry and log
storage. **Buckets only** — the scoped access keys are NOT Terraform-managed.

> **MAJOR interface change (SemVer):** earlier releases minted the Loki/Harbor
> scoped keys here with a 120-day `time_rotating` clock and exported them as
> sensitive outputs for a GitHub-secret relay. That whole surface was removed:
> the first keys are minted at bootstrap by `llz ci mint-bootstrap-objkeys`
> (bootstrap-openbao.yml) and seeded straight into OpenBao, and the in-cluster
> `linodeCredRotator` CronJob owns rotation. Terraform could not keep the keys:
> the rotator drains same-labeled keys, so a TF-tracked key gets drained and
> then recreated on the next apply — a permanent tug-of-war (see
> `docs/designs/linode-credential-rotator.md`). Removed: the two
> `linode_object_storage_key` resources, `time_rotating`,
> `obj_key_rotation_days`, and the `loki_*`/`harbor_registry_*_key` +
> `loki_key_rotates_at` outputs.

Extracted from the `object-storage/` root config so a sibling
system team can provision the same registry/telemetry storage by setting values
instead of copying YAML. The operational scars travel with the module:

- **No native `force_destroy`.** The Linode provider's
  `linode_object_storage_bucket` does not support auto-emptying on destroy, so
  buckets must be drained before `terraform destroy` — the destroy-time drain
  step in `.github/workflows/terraform.yml` mints a temporary scoped key
  (`llz ci temp-objkey`) for the sweep and deletes it afterwards.
- **Separate scoped keys** for Harbor vs Loki (minted outside TF) so a leak on
  one side doesn't expose the other.

## What it deploys

| Resource | Count | Notes |
|---|---|---|
| `linode_object_storage_bucket` | 4 | `<prefix>-harbor-registry-<suffix>`, `<prefix>-loki-{chunks,ruler,admin}-<suffix>` |

## Inputs

| Variable | Type | Default | Description |
|---|---|---|---|
| `region_suffix` | string | — | deployment/env name (e.g. `primary`, `e2e`) — appended to all labels |
| `obj_cluster` | string | — | Linode OBJ cluster id (e.g. `us-ord-1`) |
| `label_prefix` | string | `"platform"` | Bucket label prefix. **Override per sibling deployment** so labels don't collide. Key labels (minted by llz) mirror it. |

## Outputs

`bucket_names`, `harbor_registry_bucket`, `s3_endpoint`.

## Provider

Inherits the `linode` provider from the calling root (configure
`provider "linode" { token = … }` there).

## Usage

```hcl
provider "linode" {
  token = var.linode_token
}

module "object_storage" {
  source = "git::ssh://git@github.com/akamai-consulting/lke-landing-zone.git//terraform-modules/llz-object-storage?ref=v0.1.0"

  region_suffix = "primary"
  obj_cluster   = "us-ord-1"
  label_prefix  = "acme" # your org/deployment identity
}
```

See [`modules/RELEASING.md`](../RELEASING.md) for the tag/version contract.
