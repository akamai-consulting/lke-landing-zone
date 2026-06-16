locals {
  # Linode firewall labels are capped at 32 chars; truncate the cluster label
  # to 26 to always leave room for the "-nodes" suffix.
  firewall_label = var.firewall_label != "" ? var.firewall_label : "${substr(var.cluster_label, 0, 26)}-nodes"
}

# ── Networking ────────────────────────────────────────────────────────────────

resource "linode_vpc" "this" {
  label       = "${var.cluster_label}-vpc"
  region      = var.region
  description = "VPC for LKE Enterprise cluster ${var.cluster_label}"
}

# A subnet create issued immediately after its parent VPC can hit a Linode
# eventual-consistency window where the just-created VPC is not yet authorized
# for child operations — the API returns a transient `[403] Unauthorized` that
# Terraform treats as terminal (4xx is not retried), failing the whole apply.
# Give the VPC a few seconds to propagate before creating the subnet. time_sleep
# only delays on CREATE, so steady-state applies are unaffected.
resource "time_sleep" "vpc_settle" {
  depends_on      = [linode_vpc.this]
  create_duration = "15s"
}

resource "linode_vpc_subnet" "nodes" {
  vpc_id = linode_vpc.this.id
  label  = "${var.cluster_label}-nodes"
  ipv4   = var.vpc_subnet_cidr

  depends_on = [time_sleep.vpc_settle]
}

# ── Node firewall (bootstrap baseline) ───────────────────────────────────────

module "node_firewall" {
  source = "../llz-node-firewall"

  label           = local.firewall_label
  vpc_subnet_cidr = var.vpc_subnet_cidr
  tags            = var.tags

  github_runner_ipv4_cidrs = var.github_runner_ipv4_cidrs
  github_runner_ipv6_cidrs = var.github_runner_ipv6_cidrs
}

# ── LKE Enterprise cluster ────────────────────────────────────────────────────

resource "linode_lke_cluster" "this" {
  label       = var.cluster_label
  region      = var.region
  k8s_version = var.k8s_version
  tier        = "enterprise"
  tags        = var.tags
  # Bind the cluster to OUR VPC. Both vpc_id and subnet_id must be set together:
  # passing subnet_id alone (vpc_id omitted) does NOT attach this VPC — LKE-E
  # silently provisions its own "lke<clusterID>" VPC instead, leaving the
  # linode_vpc.this above orphaned. Each cycle then leaks a "<label>-vpc" until
  # the account hits its VPC quota and new cluster creates hang forever in
  # "Still creating…" (no VPC available). See the provider's enterprise example.
  vpc_id    = linode_vpc.this.id
  subnet_id = linode_vpc_subnet.nodes.id

  control_plane {
    high_availability  = var.control_plane_high_availability
    audit_logs_enabled = var.control_plane_audit_logs_enabled
    acl {
      enabled = true
      addresses {
        # Runner CIDRs are merged at bootstrap time so they can reach the API
        # server. The firewall-controller takes over ACL management after init,
        # which is why ignore_changes is set below.
        ipv4 = concat(var.control_plane_acl_ipv4, module.node_firewall.acl_cidrs_ipv4)
        ipv6 = concat(var.control_plane_acl_ipv6, module.node_firewall.acl_cidrs_ipv6)
      }
    }
  }

  # The firewall-controller updates the control-plane ACL on every reconcile
  # via the Linode API. Ignore ACL drift so Terraform does not overwrite the
  # controller's live state on subsequent applies.
  #
  # `pool` is also ignored: node pools are owned by the separate
  # linode_lke_node_pool resource (module.node_pool). The Linode API still
  # echoes pools back on the cluster object, and without this guard Terraform
  # treats them as drift and nulls them out — which the API interprets as
  # "delete the pool" (observed in practice: a live pool was destroyed
  # this way).
  lifecycle {
    ignore_changes = [control_plane[0].acl, pool]
  }

  # Fail fast: force the node firewall (and, via vpc_id/subnet_id above, the VPC
  # + subnet) to be created BEFORE the cluster. The cluster only references the
  # firewall module through acl_cidrs_ipv4/ipv6, which are pass-throughs of input
  # variables — so without this depends_on Terraform creates the firewall and the
  # cluster in PARALLEL. A stale/duplicate firewall label then 400s immediately
  # ("Label must be unique among your Cloud Firewalls") while the ~20-30 min
  # cluster create is already in flight. Ordering firewall/VPC first surfaces
  # those cheap, instant failures before the expensive create begins.
  depends_on = [module.node_firewall]
}

# ── Kubeconfig (optional) ─────────────────────────────────────────────────────

resource "local_sensitive_file" "kubeconfig" {
  count           = var.kubeconfig_path != "" ? 1 : 0
  filename        = var.kubeconfig_path
  content         = base64decode(linode_lke_cluster.this.kubeconfig)
  file_permission = "0600"
}
