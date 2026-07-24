# ─────────────────────────────────────────────────────────────────────────────
# Phase-3 dogfood cutover — see terraform-modules/RELEASING.md. COMPLETE: every
# module this root consumes resolves from its published git:: source pinned to
# the umbrella tag. The relative path stays commented beneath each as the
# local-dev override.
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

# Shared-VPC attach: when the env declares cluster.network.vpc, `llz render` sets
# vpc_network to that VPC's label. Look it up (the vpc root created it first — see
# apply-vpc → apply-cluster ordering) and pass its id to the module so this cluster
# attaches its subnet to the shared VPC instead of creating a dedicated one. Empty
# vpc_network → the module creates a dedicated <cluster_label>-vpc (the default).
data "linode_vpcs" "shared" {
  count = var.vpc_network == "" ? 0 : 1
  filter {
    name   = "label"
    values = [var.vpc_network]
  }
}

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
  vpc_id          = var.vpc_network == "" ? "" : tostring(data.linode_vpcs.shared[0].vpcs[0].id)

  control_plane_high_availability  = var.control_plane_high_availability
  control_plane_audit_logs_enabled = var.control_plane_audit_logs_enabled
  apl_enabled                      = var.apl_enabled

  firewall_label           = var.firewall_label
  github_runner_ipv4_cidrs = var.github_runner_ipv4_cidrs
  github_runner_ipv6_cidrs = var.github_runner_ipv6_cidrs
}

# ── Node pool ─────────────────────────────────────────────────────────────────
#
# Formerly the llz-pool module: a single linode_lke_node_pool behind eleven
# pass-through variables, with no locals or computed logic. Inlined — this root
# already declares every input it took.

# State migration for instances upgrading across the inline: keeps the existing
# pool in state instead of destroy/recreate. That matters here — node_type is
# ForceNew, so a missed move would rebuild the whole pool.
moved {
  from = module.node_pool.linode_lke_node_pool.this
  to   = linode_lke_node_pool.this
}

resource "linode_lke_node_pool" "this" {
  cluster_id = module.cluster.cluster_id
  label      = var.node_pool_label
  type       = var.node_type
  tags       = concat(var.tags, [var.node_pool_label])

  # Static count is ignored by the API when autoscaling is enabled.
  node_count = var.autoscaler_enabled ? null : var.node_count

  # Security invariants — not configurable. These were the llz-pool module's
  # reason to exist; keep them hardcoded rather than re-exposing them as knobs.
  firewall_id     = module.cluster.node_firewall_id
  disk_encryption = "enabled"

  labels = {
    environment = "shared"
    role        = "observability"
  }

  dynamic "autoscaler" {
    for_each = var.autoscaler_enabled ? [1] : []
    content {
      min = var.autoscaler_min
      max = var.autoscaler_max
    }
  }
}
