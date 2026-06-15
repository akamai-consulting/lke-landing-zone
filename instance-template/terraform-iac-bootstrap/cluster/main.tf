# ─────────────────────────────────────────────────────────────────────────────
# Phase-3 dogfood cutover (templatization-plan §6/§10, modules/RELEASING.md).
# node_pool is CUT OVER to its published git:: source (the canary). The remaining
# modules each keep a commented `git::?ref=` line staged above an active relative
# `source` — they still resolve locally so `terraform plan` keeps working. To cut
# the next one over: PUSH its tag first, then swap the two `source` lines (comment
# the relative one, uncomment the git:: one).
# Order: node_pool (done) → cluster.
#
# EAA / control-plane-ACL CIDRs are no longer fetched here. The old GitHub
# (-Enterprise) inventory fetch (llz-acl-cidr-sync) and its Linode-template
# plan-time twin (llz-eaa-firewall-cidrs) were removed: the in-cluster
# cloud-firewall-controller now resolves EAA/bastion CIDRs from the Linode
# firewall template via the Linode API and owns BOTH the node firewall and the
# LKE control-plane ACL on every reconcile (with a committed firewall_rules/
# fallback). Terraform only seeds the bootstrap control-plane ACL from the static
# github_runner_*_cidrs tfvars so the bootstrapping runner can reach the API
# server before the controller takes over.
# ─────────────────────────────────────────────────────────────────────────────

module "cluster" {
  # checkov:skip=CKV_TF_1: First-party module sources pin to immutable-by-convention
  # SemVer tags (terraform-modules/RELEASING.md — tags are never moved), which are the
  # human-readable version contract; a raw commit SHA here would defeat that scheme.
  source = "git::ssh://git@github.com/<@ upstream_org @>/lke-landing-zone.git//terraform-modules/llz-cluster?ref=<@ llz_version @>"
  # source = "../../terraform-modules/llz-cluster"

  cluster_label   = var.cluster_label
  region          = var.region
  k8s_version     = var.k8s_version
  tags            = var.tags
  vpc_subnet_cidr = var.vpc_subnet_cidr

  control_plane_high_availability  = var.control_plane_high_availability
  control_plane_audit_logs_enabled = var.control_plane_audit_logs_enabled

  firewall_label           = var.firewall_label
  github_runner_ipv4_cidrs = var.github_runner_ipv4_cidrs
  github_runner_ipv6_cidrs = var.github_runner_ipv6_cidrs

  # cluster-bootstrap workspace consumes the kubeconfig via terraform_remote_state
  # output (kubeconfig_raw); we do not need a local copy in this workspace.
  kubeconfig_path = ""
}

module "node_pool" {
  # This root dogfoods the PUBLISHED modules from their tagged git:: sources per
  # terraform-modules/RELEASING.md. Every module pins to the same umbrella tag,
  # rendered here as the copier `llz_version` that `llz new`/`llz upgrade` set to
  # the version of the llz binary you ran. The relative path is kept commented as
  # the local-dev override.
  #
  # checkov:skip=CKV_TF_1: First-party module sources pin to immutable-by-convention
  # SemVer tags (terraform-modules/RELEASING.md — tags are never moved), which are the
  # human-readable version contract; a raw commit SHA here would defeat that scheme.
  source = "git::ssh://git@github.com/<@ upstream_org @>/lke-landing-zone.git//terraform-modules/llz-pool?ref=<@ llz_version @>"
  # source = "../../terraform-modules/llz-pool"

  cluster_id       = module.cluster.cluster_id
  node_firewall_id = module.cluster.node_firewall_id

  label     = var.node_pool_label
  node_type = var.node_type
  tags      = concat(var.tags, [var.node_pool_label])

  node_labels = {
    environment = "shared"
    role        = "observability"
  }

  node_count         = var.node_count
  autoscaler_enabled = var.autoscaler_enabled
  autoscaler_min     = var.autoscaler_min
  autoscaler_max     = var.autoscaler_max
}
