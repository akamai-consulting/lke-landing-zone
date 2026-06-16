# `object-storage`

Linode Object Storage buckets + scoped access keys for a platform's container
registry and log storage, with a declarative 120-day key-rotation clock.

Extracted from the `object-storage/` root config so a sibling
system team can provision the same registry/telemetry storage by setting values
instead of copying YAML. The operational scars travel with the module:

- **No native `force_destroy`.** The Linode provider's
  `linode_object_storage_bucket` does not support auto-emptying on destroy, so
  buckets must be drained (`aws s3 rm --recursive`) before `terraform destroy` —
  see the destroy-time drain step in `.github/workflows/terraform.yml`.
- **Rotation = destroy+recreate.** OBJ keys have no native expiry; the 120-day
  SLA is enforced by a `time_rotating` resource that forces key replacement.
  Reseeding the new credentials into OpenBao is a **manual** hop (see `next_steps`).
- **Separate scoped keys** for Harbor vs Loki so a leak on one side doesn't
  expose the other.

## What it deploys

| Resource | Count | Notes |
|---|---|---|
| `linode_object_storage_bucket` | 4 | `<prefix>-harbor-registry-<suffix>`, `<prefix>-loki-{chunks,ruler,admin}-<suffix>` |
| `linode_object_storage_key` | 2 | Loki key (3 buckets, read_write), Harbor key (1 bucket, read_write) |
| `time_rotating` | 1 | drives the 120-day forced-rotation clock |

## Inputs

| Variable | Type | Default | Description |
|---|---|---|---|
| `region_suffix` | string | — | `primary` \| `secondary` \| `staging` \| `lab` — appended to all labels |
| `obj_cluster` | string | — | Linode OBJ cluster id (e.g. `us-ord-1`) |
| `obj_key_rotation_days` | number | `120` | Max key age before forced rotation (≤120, Guidelines cap) |
| `label_prefix` | string | `"platform"` | Bucket/key label prefix. **Override per sibling deployment** so labels don't collide. |

## Outputs

`loki_access_key` / `loki_secret_key` (sensitive), `bucket_names`,
`harbor_registry_bucket`, `harbor_registry_access_key` /
`harbor_registry_secret_key` (sensitive), `s3_endpoint`, `loki_key_rotates_at`,
`next_steps` (post-apply / post-rotation reseed checklist).

## Provider

Inherits the `linode` provider from the calling root (configure
`provider "linode" { token = … }` there). The `time` provider is config-less.

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
