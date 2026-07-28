# llz-databases

A reusable Terraform module that provisions **one shared, VPC-attached Linode
Managed PostgreSQL cluster** per deployment. Downstream application platforms
carve per-app logical databases + roles out of this single cluster (e.g. with
Crossplane `provider-sql`), reaching it **over the VPC** — the cluster has no
public endpoint.

It is the database analog of [`llz-object-storage`](../llz-object-storage): the
embedded `databases` root (`tools/internal/tfroots/roots/databases`) is a thin
consumer, and a sibling system team can provision the same cluster by calling
this module with their own `label_prefix` + VPC.

The admin credentials this module outputs are seeded into OpenBao at
`secret/platform/db-admin` by `llz ci seed-db-admin` at bootstrap (never in
Terraform-committed state visible to operators; `root_password`/`ca_cert` are
`sensitive`).

## Why the `_v2` resource

`linode_database_postgresql_v2` (plugin-framework based) is required for the
`private_network` attachment that restricts the cluster to a VPC. Note its
`private_network`/`updates` are nested **attributes** (`= { … }`), not blocks.

## Inputs

| Name | Type | Default | Description |
|---|---|---|---|
| `region_suffix` | string | — | Deployment/env discriminator (e.g. `primary`); appended to the label. Format-validated; rejects `your-env`. |
| `region` | string | — | Linode geographic region (e.g. `us-ord`). **Must match the VPC's region.** |
| `vpc_id` | number | — | VPC to attach (restrict) the database to. |
| `subnet_id` | number | — | Subnet in `vpc_id` to attach to. |
| `public_access` | bool | `false` | Allow clients outside the VPC (a public IP). Keep `false`. |
| `engine_version` | string | `"16"` | Major PostgreSQL version (→ `engine_id = "postgresql/<v>"`). |
| `db_type` | string | `"g6-dedicated-2"` | Linode Managed DB node type. |
| `cluster_size` | number | `2` | `1` single, `2`/`3` HA (standbys). |
| `label_prefix` | string | `"platform"` | Label prefix (org/deployment identity). |
| `maintenance` | object | Sun 08:00 UTC, 1h | Weekly patch window. |

## Outputs

| Name | Sensitive | Description |
|---|---|---|
| `database_id` | no | Linode Managed Database ID. |
| `label` | no | Cluster label. |
| `host` | no | Primary (VPC-internal) host → `db-admin.endpoint`. |
| `host_standby` | no | Standby host (HA). |
| `port` | no | → `db-admin.port`. |
| `root_username` | no | → `db-admin.username`. |
| `root_password` | **yes** | → `db-admin.password`. |
| `ca_cert` | **yes** | Base64 CA → `db-admin.ca`. |
| `engine_version` | no | Provisioned engine version. |

## Requirements

| | Version |
|---|---|
| terraform | >= 1.5.0 |
| linode/linode | ~> 3.11 |
