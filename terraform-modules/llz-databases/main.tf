# ── Managed Postgres, VPC-attached ────────────────────────────────────────────
# ONE Linode Managed Postgres cluster, placed INSIDE the cluster VPC and
# restricted to it (no public endpoint). Downstream app platforms carve per-app
# logical databases + roles out of a cluster with Crossplane provider-sql,
# reaching it over the private network — the admin credentials this module
# outputs are seeded into OpenBao at secret/platform/db-admin/<name> by
# `llz ci seed-db-admin` at bootstrap (the analog of mint-bootstrap-objkeys).
#
# ONE CLUSTER PER MODULE CALL is deliberate. A deployment may want zero clusters
# (the common case), one shared cluster, or several — e.g. a shared tenant DB
# plus a separately-sized one for an app with its own IOPS/version needs. The
# fan-out lives in the caller: the `databases` root does `for_each = var.databases`
# over a map keyed by cluster name, so each cluster gets its own module instance
# and its own address in state (module.databases["shared"]) — adding or removing
# one never re-plans the others.
#
# WHY a module (not just the root): a sibling system team can provision the same
# clusters by calling this with their own label_prefix + VPC, exactly like
# llz-object-storage. The v2 resource (vs the deprecated linode_database_postgresql)
# is required for the private_network attachment.
locals {
  # The provider composes the engine identity as "<engine>/<version>".
  engine_id = "postgresql/${var.engine_version}"
}

resource "linode_database_postgresql_v2" "this" {
  # UNVERIFIED: the composed label's maximum length. The provider declares NO
  # client-side length validator on linode_database_postgresql_v2.label (checked
  # against the provider source — unlike e.g. firewall rulesets, which do carry a
  # LengthBetween(3, 32)), so whatever the API enforces surfaces only at APPLY.
  # name and region_suffix are each capped at 31 by their own validations, so this
  # can compose a label up to ~72 characters. Deliberately NOT guarded with a
  # guessed bound — a wrong limit would reject valid labels — but if a long
  # name+env pair ever fails at create with a label error, this is why.
  label     = "${var.label_prefix}-${var.name}-${var.region_suffix}"
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
