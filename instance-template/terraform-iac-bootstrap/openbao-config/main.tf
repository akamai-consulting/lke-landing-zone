# Thin consumer of the reusable `llz-openbao` module
# (terraform-modules/llz-openbao) — the KV v2 engine, AppRole +
# Kubernetes auth, CI read policy, and AppRole-rotation wiring now live in the
# module. The in-repo deployment uses the module defaults (ci_role_name
# "platform-ci", role_id "platform-ci", and the full ci_read_paths secret tree), so the
# resulting OpenBao config is unchanged. A sibling system team overrides
# ci_role_name / ci_read_paths to bring its own. See modules/llz-openbao/README.md.
module "openbao_config" {
  # checkov:skip=CKV_TF_1: First-party module sources pin to immutable-by-convention
  # SemVer tags (terraform-modules/RELEASING.md — tags are never moved), which are the
  # human-readable version contract; a raw commit SHA here would defeat that scheme.
  source = "git::ssh://git@github.com/<@ upstream_org @>/lke-landing-zone.git//terraform-modules/llz-openbao?ref=<@ llz_version @>"
  # source = "../../terraform-modules/llz-openbao"

  kubernetes_host = var.kubernetes_host
}
