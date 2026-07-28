# llz-databases

A reusable Terraform module that provisions **one VPC-attached Linode Managed
PostgreSQL cluster**. Downstream application platforms carve per-app logical
databases + roles out of a cluster (e.g. with Crossplane `provider-sql`),
reaching it **over the VPC** — the cluster has no public endpoint.

It is the database analog of [`llz-object-storage`](../llz-object-storage): the
embedded `databases` root (`tools/internal/tfroots/roots/databases`) is a thin
consumer, and a sibling system team can provision the same clusters by calling
this module with their own `label_prefix` + VPC.

The admin credentials this module outputs are seeded into OpenBao at
`secret/infra/db-admin/<name>` by `llz ci seed-db-admin` at bootstrap (never in
Terraform-committed state visible to operators; `root_password`/`ca_cert` are
`sensitive`).

## One cluster per module call — the 0-n fan-out is the caller's

A deployment may want **zero** clusters (the common case), one shared cluster, or
several — e.g. a shared tenant DB plus a separately-sized one for an app with its
own version or IOPS needs. This module is deliberately singular; the `databases`
root does the fan-out:

```hcl
module "databases" {
  source   = "…//terraform-modules/llz-databases?ref=<tag>"
  for_each = var.databases          # map(object({ … })), default {}

  name          = each.key
  region_suffix = var.region_suffix
  region        = each.value.region
  vpc_id        = each.value.vpc_id
  subnet_id     = each.value.subnet_id
  # …
}
```

Keying on the **name** rather than a list position is what makes a cluster's
identity stable: the key is simultaneously its label segment, its state address
(`module.databases["shared"]`), and its OpenBao path, so adding or removing one
cluster never re-plans its siblings. Under a list, deleting the first of three
would shift the other two onto each other's state.

## Why the `_v2` resource

`linode_database_postgresql_v2` (plugin-framework based) is required for the
`private_network` attachment that restricts the cluster to a VPC. Note its
`private_network`/`updates` are nested **attributes** (`= { … }`), not blocks.

Their nested values are **not** checked by `tofu validate` — only by `plan`. That
is how `updates.day_of_week` shipped as the string `"sunday"` when the provider
wants the number `7`: validate passed, every plan would have failed. `make
tf-validate`/`tf-validate-roots` do not catch this class; a plan does.

## Inputs

| Name | Type | Default | Description |
|---|---|---|---|
| `name` | string | `"postgres"` | This cluster's name within the deployment — the 0-n discriminator, and the middle label segment. Format-validated. |
| `region_suffix` | string | — | Deployment/env discriminator (e.g. `primary`); appended to the label. Format-validated; rejects `your-env`. |
| `region` | string | — | Linode geographic region (e.g. `us-ord`). **Must match the VPC's region.** |
| `vpc_id` | number | — | VPC to attach (restrict) the database to. |
| `subnet_id` | number | — | Subnet in `vpc_id` to attach to. |
| `public_access` | bool | `false` | Allow clients outside the VPC (a public IP). Keep `false`. |
| `engine_version` | string | `"16"` | Major PostgreSQL version (→ `engine_id = "postgresql/<v>"`). |
| `db_type` | string | `"g6-dedicated-2"` | Linode Managed DB node type. |
| `cluster_size` | number | `2` | `1` single, `2`/`3` HA (standbys). |
| `label_prefix` | string | `"platform"` | Label prefix (org/deployment identity). |
| `maintenance` | object | day 7 (Sun) 08:00 UTC, 1h | Weekly patch window. `day_of_week` is **numeric**: 1 = Monday … 7 = Sunday. |

The label is `"<label_prefix>-<name>-<region_suffix>"` — e.g.
`platform-shared-primary`.

## Outputs

| Name | Sensitive | Description |
|---|---|---|
| `database_id` | no | Linode Managed Database ID. |
| `label` | no | Cluster label. |
| `host` | no | Primary (VPC-internal) host → `db-admin/<name>.endpoint`. |
| `host_standby` | no | Standby host (HA). |
| `port` | no | → `db-admin/<name>.port`. |
| `root_username` | **yes** | → `db-admin/<name>.username`. Provider-marked sensitive. |
| `root_password` | **yes** | → `db-admin/<name>.password`. |
| `ca_cert` | **yes** | Base64 CA → `db-admin/<name>.ca`. |
| `engine_version` | no | Provisioned engine version. |

The **root** re-exports these as maps keyed by cluster name (`hosts`, `ports`,
`database_ids`, `labels`) plus one sensitive `connections` map holding the full
`{ endpoint, port, username, password, ca }` per cluster — the single read
`llz ci seed-db-admin` needs, so it cannot pair one cluster's host with another's
password.

## Requirements

| | Version |
|---|---|
| terraform | >= 1.5.0 |
| linode/linode | ~> 3.11 |
