variable "cluster_label" {
  description = "Unique label for the LKE Enterprise cluster."
  type        = string
}

variable "region" {
  description = "Linode region for the cluster, for example us-lax or us-sea."
  type        = string
}

variable "ha_role" {
  description = <<-EOT
    This deployment's role in its OpenBao HA topology:
      • "active"     — the cluster that provisions Harbor robots and receives
                       its standby peer's CA cross-region.
      • "standby"    — mirrors the active: seeds Harbor creds from the active's
                       published secrets and ships its CA to the active.
      • "standalone" — a single, self-contained OpenBao (no peer, single-write,
                       local Harbor). The default — most deployments are standalone.
    "active"/"standby" must come in pairs sharing one ha_group.
  EOT
  type        = string
  default     = "standalone"
  validation {
    condition     = contains(["active", "standby", "standalone"], var.ha_role)
    error_message = "ha_role must be one of: active, standby, standalone."
  }
}

# ha_role/ha_group are consumed OUT OF BAND, not by this root's HCL: `llz topology`
# parses them straight out of the rendered <env>.tfvars text. They are declared here
# so (a) terraform does not reject the tfvars for an undeclared variable and (b) the
# pairing validation below fails a bad HA config at plan time.
# tflint-ignore: terraform_unused_declarations
variable "ha_group" {
  description = <<-EOT
    HA pair identifier. The two deployments sharing one non-empty ha_group are
    OpenBao peers (one active + one standby). Must be empty for ha_role =
    "standalone", and non-empty for "active"/"standby".
  EOT
  type        = string
  default     = ""
  validation {
    condition     = (var.ha_role == "standalone") == (var.ha_group == "")
    error_message = "ha_group must be set for active/standby and empty for standalone."
  }
}

variable "k8s_version" {
  description = "Enterprise LKE Kubernetes version, for example v1.31.8+lke5."
  type        = string
}

variable "tags" {
  description = "Tags applied to the cluster (and the node pool/firewall/VPC). The spec render emits this from spec.cluster.tags; default empty so an instance that declares no tags is not silently stamped with a leftover label."
  type        = list(string)
  default     = []
}

variable "node_pool_label" {
  description = "Label for the default node pool. Not account-unique, so it does not collide, but it MUST match llz's DefaultNodePoolLabel (tools/internal/terraform/tfvars.go) — that is the value llz's import/teardown reconstructs when <region>.tfvars carries no node_pool_label (the spec render does not emit one). A divergent default here makes `llz ci tf-import` look for the wrong pool label and skip adopting the live pool. Keep it under 16 chars — longer labels have left LKE nodes stuck never joining the pool (see the validation below)."
  type        = string
  default     = "platform-pool"

  # A node-pool label of 16+ chars has wedged LKE node provisioning: the pool is
  # created (the API returns in seconds) but the nodes never register/become
  # Ready, so the cluster apply "succeeds" onto an empty pool and every downstream
  # workload (apl-operator, then helm_release.apl) hangs until its wait times out.
  # Fail fast at plan/validate instead. Keep this in lockstep with DefaultNodePoolLabel.
  validation {
    condition     = length(var.node_pool_label) < 16
    error_message = "node_pool_label must be < 16 characters (LKE nodes fail to join the pool with longer labels); got \"${var.node_pool_label}\" (${length(var.node_pool_label)} chars)."
  }
}

variable "node_type" {
  description = "Linode instance type for worker nodes."
  type        = string
  default     = "g8-dedicated-8-4"
}

variable "node_count" {
  description = "Number of nodes in the default node pool when autoscaling is disabled. Default 5: apl-core + the full support-plane (OpenBao HA ×3, ESO, Argo, cert-automation, firewall) does not fit on 3× g8-dedicated-8-4 — OpenBao followers go Pending on Insufficient memory / max Block Storage volume count, stalling Bootstrap OpenBao. node_count is NOT ForceNew (scales the pool live). That sizing was measured against apl-core 5.0.0; apl-core 6.x reduced platform requests (0.8 CPU / 1 GB RAM / 6 PVs less, gitea gone) — retest 4 (or 3) nodes on a v6 lab bootstrap before relaxing this default."
  type        = number
  default     = 5
}

variable "control_plane_high_availability" {
  description = "Whether to enable LKE control plane HA."
  type        = bool
  default     = true
}

variable "control_plane_audit_logs_enabled" {
  description = "Whether to enable control plane audit logs."
  type        = bool
  default     = true
}

variable "autoscaler_enabled" {
  description = "Whether to enable autoscaling for the default node pool."
  type        = bool
  default     = false
}

variable "autoscaler_min" {
  description = "Minimum node count when autoscaling is enabled."
  type        = number
  default     = 3
}

variable "autoscaler_max" {
  description = "Maximum node count when autoscaling is enabled."
  type        = number
  default     = 6
}

variable "vpc_subnet_cidr" {
  description = "IPv4 CIDR block for the VPC subnet used by LKE worker nodes. LKE-E requires /13 or /14."
  type        = string
  default     = "10.0.0.0/13"
}

variable "vpc_network" {
  description = <<-EOT
    Label of a SHARED VPC to attach this cluster to (the spec.networks key the env
    references via cluster.network.vpc; set by `llz render`). Empty (the default)
    creates a dedicated <cluster_label>-vpc. The shared VPC must already exist — the
    vpc root applies before the clusters (apply-vpc → apply-cluster).
  EOT
  type        = string
  default     = ""
}

variable "firewall_label" {
  description = "Label for the LKE node firewall (account-unique; capped at 32 chars). Leave empty (the default) to derive a unique \"<cluster_label[:26]>-nodes\" from cluster_label — set it explicitly only to pin a specific name. A non-empty hardcoded default here would make every cluster that doesn't override it collide on the same firewall label."
  type        = string
  default     = ""
}

variable "github_runner_ipv4_cidrs" {
  description = "IPv4 CIDRs for GitHub Actions runners. Adds NodePort inbound rules to the node firewall and injects the CIDRs into the LKE-E control-plane ACL at bootstrap time."
  type        = list(string)
  default     = []
}

variable "github_runner_ipv6_cidrs" {
  description = "IPv6 CIDRs for GitHub Actions runners. Adds NodePort inbound rules to the node firewall and injects the CIDRs into the LKE-E control-plane ACL at bootstrap time."
  type        = list(string)
  default     = []
}

variable "apl_enabled" {
  description = "Enable Linode managed App Platform (apl_enabled). See ADR 0005 / spec cluster.bootstrap.managedAppPlatform."
  type        = bool
  default     = false
}
