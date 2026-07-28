# This root is a thin consumer of the reusable `databases` module
# (terraform-modules/llz-databases) — the shared, VPC-attached Managed Postgres
# lives in the module so a sibling system team can provision the same cluster by
# calling it with their own label_prefix + VPC. The module's scar-comments (v2
# resource, VPC-only posture, admin-cred seeding) travel with the code. See
# terraform-modules/llz-databases/README.md.
#
# label_prefix is intentionally left at the module default ("platform") so the
# in-repo deployment's database label is unchanged.
module "databases" {
  # checkov:skip=CKV_TF_1: First-party module sources pin to immutable-by-convention
  # SemVer tags (terraform-modules/RELEASING.md — tags are never moved), which are the
  # human-readable version contract; a raw commit SHA here would defeat that scheme.
  source = "git::ssh://git@github.com/<@ upstream_org @>/lke-landing-zone.git//terraform-modules/llz-databases?ref=<@ llz_version @>"
  # source = "../../terraform-modules/llz-databases"

  region_suffix  = var.region_suffix
  region         = var.region
  vpc_id         = var.vpc_id
  subnet_id      = var.subnet_id
  engine_version = var.engine_version
  db_type        = var.db_type
  cluster_size   = var.cluster_size
}
