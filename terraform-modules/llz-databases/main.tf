# ── Shared Managed Postgres, VPC-attached ─────────────────────────────────────
# One Linode Managed Postgres cluster per deployment, placed INSIDE the cluster
# VPC and restricted to it (no public endpoint). Downstream app platforms carve
# per-app logical databases + roles out of this one cluster with Crossplane
# provider-sql, reaching it over the private network — the admin credentials this
# module outputs are seeded into OpenBao at secret/platform/db-admin by
# `llz ci seed-db-admin` at bootstrap (the analog of mint-bootstrap-objkeys).
#
# WHY a module (not just the root): a sibling system team can provision the same
# shared cluster by calling this with their own label_prefix + VPC, exactly like
# llz-object-storage. The v2 resource (vs the deprecated linode_database_postgresql)
# is required for the private_network attachment.
locals {
  # The provider composes the engine identity as "<engine>/<version>".
  engine_id = "postgresql/${var.engine_version}"
}

resource "linode_database_postgresql_v2" "shared" {
  label     = "${var.label_prefix}-postgres-${var.region_suffix}"
  engine_id = local.engine_id
  region    = var.region
  type      = var.db_type

  # cluster_size 2/3 provisions standby node(s) for HA; 1 is single-node.
  cluster_size = var.cluster_size

  # VPC attachment — the security boundary. With public_access=false the cluster
  # has NO public IP and is reachable only from within vpc_id/subnet_id.
  # (linode_database_postgresql_v2 is a plugin-framework resource, so these are
  # nested ATTRIBUTES assigned with `=`, not HCL blocks.)
  private_network = {
    vpc_id        = var.vpc_id
    subnet_id     = var.subnet_id
    public_access = var.public_access
  }

  updates = {
    frequency   = "weekly"
    day_of_week = var.maintenance.day_of_week
    hour_of_day = var.maintenance.hour_of_day
    duration    = var.maintenance.duration
  }
}
