# This root is a thin consumer of the reusable `databases` module
# (terraform-modules/llz-databases) — the VPC-attached Managed Postgres lives in
# the module so a sibling system team can provision the same clusters by calling
# it with their own label_prefix + VPC. The module's scar-comments (v2 resource,
# VPC-only posture, admin-cred seeding) travel with the code. See
# terraform-modules/llz-databases/README.md.
#
# The module provisions ONE cluster; the 0-n fan-out is this for_each. Each
# cluster is addressed in state by its map key (module.databases["shared"]), so
# a deployment can add or drop a cluster without touching the others.
#
# label_prefix is intentionally left at the module default ("platform") so the
# in-repo deployment's database labels are unchanged.
module "databases" {
  # checkov:skip=CKV_TF_1: First-party module sources pin to immutable-by-convention
  # SemVer tags (terraform-modules/RELEASING.md — tags are never moved), which are the
  # human-readable version contract; a raw commit SHA here would defeat that scheme.
  source = "git::ssh://git@github.com/<@ upstream_org @>/lke-landing-zone.git//terraform-modules/llz-databases?ref=<@ llz_version @>"
  # source = "../../terraform-modules/llz-databases"

  for_each = var.databases

  name          = each.key
  region_suffix = var.region_suffix

  region         = each.value.region
  vpc_id         = each.value.vpc_id
  subnet_id      = each.value.subnet_id
  engine_version = each.value.engine_version
  db_type        = each.value.db_type
  cluster_size   = each.value.cluster_size
}
