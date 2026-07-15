# This root is a thin consumer of the reusable `object-storage` module
# (terraform-modules/llz-object-storage) — the OBJ buckets live in the module
# so a sibling system team
# can provision the same registry/telemetry storage by calling it with their own
# label_prefix. The module's scar-comments (force_destroy quirk, rotation model,
# Harbor wiring) travel with the code. See modules/object-storage/README.md.
#
# label_prefix is intentionally left at the module default ("platform") so the
# in-repo deployment's bucket labels are unchanged.
module "object_storage" {
  # checkov:skip=CKV_TF_1: First-party module sources pin to immutable-by-convention
  # SemVer tags (terraform-modules/RELEASING.md — tags are never moved), which are the
  # human-readable version contract; a raw commit SHA here would defeat that scheme.
  source = "git::ssh://git@github.com/<@ upstream_org @>/lke-landing-zone.git//terraform-modules/llz-object-storage?ref=<@ llz_version @>"
  # source = "../../terraform-modules/llz-object-storage"

  region_suffix = var.region_suffix
  obj_cluster   = var.obj_cluster
}
