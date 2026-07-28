variable "region_suffix" {
  description = "Deployment suffix appended to the database label — the lowercase deployment/env name (e.g. primary, secondary, staging, lab, e2e, or an adopter's own env). Despite the name this is the deployment discriminator, not strictly a geographic region; it must match the cluster workspace deployment. Environments are created dynamically, so this is validated by format, not a fixed list."
  type        = string

  validation {
    # Same env-name shape the object-storage module enforces; reject the shipped
    # placeholder so an unscaffolded tfvars fails loudly.
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.region_suffix)) && var.region_suffix != "your-env"
    error_message = "region_suffix must be the lowercase deployment name matching the cluster workspace (^[a-z][a-z0-9-]{1,30}$) — e.g. primary, secondary, staging, lab, e2e — not the 'your-env' placeholder."
  }
}

variable "region" {
  description = "Linode region the Managed Postgres is provisioned in (e.g. us-ord). This is the geographic region — NOT the object-storage endpoint id. It MUST match the region of the VPC in var.vpc_id (a database can only attach to a VPC in its own region)."
  type        = string
}

variable "engine_version" {
  description = "Major PostgreSQL engine version. Composed into the provider's engine_id as \"postgresql/<version>\" (e.g. \"16\"). When MIGRATING data into this cluster, set it to at least the source cluster's major version — pg_restore is forward-compatible only, so a lower target fails at restore time, after the cluster is provisioned."
  type        = string
  default     = "16"
}

variable "db_type" {
  description = "Linode Managed Database node type (e.g. g6-dedicated-2, g6-nanode-1). Dedicated types are recommended for production. Run `linode-cli databases types` to list."
  type        = string
  default     = "g6-dedicated-2"
}

variable "cluster_size" {
  description = "Number of database nodes. 1 = single node; 2 or 3 = high availability with standbys. Production defaults to 2."
  type        = number
  default     = 2

  validation {
    condition     = contains([1, 2, 3], var.cluster_size)
    error_message = "cluster_size must be 1 (single) or 2/3 (HA)."
  }
}

# ── VPC attachment (the whole point of this module) ───────────────────────────
# The shared Managed Postgres is placed INSIDE the cluster's VPC and restricted
# to it: no public IP, reached only over the private network from workloads in
# the same VPC. This is the security posture the platform's DB story assumes
# (contrast a public g2a.akamaidb.net endpoint). vpc_id + subnet_id must name a
# VPC/subnet in var.region.
variable "vpc_id" {
  description = "ID of the VPC to attach (restrict) the database to. Must be in var.region — typically the same VPC the LKE cluster attaches to (the cluster/ root's VPC)."
  type        = number
}

variable "subnet_id" {
  description = "ID of the VPC subnet to attach the database to. Must belong to var.vpc_id."
  type        = number
}

variable "public_access" {
  description = "Allow clients OUTSIDE the VPC to reach the database over a public IP. Default false — VPC-only is the point. Set true only for a temporary migration/debug window."
  type        = bool
  default     = false
}

# Org/deployment identity — override per sibling deployment so two deployments
# don't collide on the global database label. Matches the object-storage module's
# label_prefix convention; the label becomes "<label_prefix>-postgres-<suffix>".
variable "label_prefix" {
  description = "Prefix for the database label. Org/deployment identity — override per sibling deployment so labels don't collide."
  type        = string
  default     = "platform"
}

# Weekly maintenance window (UTC). Managed Databases apply engine patches in this
# window; pinning it keeps disruption predictable instead of provider-default.
variable "maintenance" {
  description = "Weekly maintenance window (UTC) for engine patching."
  type = object({
    day_of_week = string # e.g. "sunday"
    hour_of_day = number # 0-23
    duration    = number # 1-3 hours
  })
  default = {
    day_of_week = "sunday"
    hour_of_day = 8
    duration    = 1
  }
}
