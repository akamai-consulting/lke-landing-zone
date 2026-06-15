variable "linode_api_version" {
  description = "Linode API version. LKE Enterprise requires v4beta."
  type        = string
  default     = "v4beta"
}

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
      • "active"     — the cluster that provisions Harbor robots, owns the
                       base-named AppRole secret, and receives its standby
                       peer's CA cross-region.
      • "standby"    — mirrors the active: seeds Harbor creds from the active's
                       published secrets, writes the _STANDBY-suffixed AppRole
                       secret, and ships its CA to the active.
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

variable "promotion_rank" {
  description = <<-EOT
    This deployment's position in the code-promotion pipeline, ascending: the
    lowest positive rank is the first stage a change promotes into (e.g. dev = 1),
    the highest is the last (e.g. prod = 3). 0 (the default) means the deployment
    is not part of any promotion pipeline. Ranks must be unique across deployments
    — the pipeline is a line, not a tie. `llz env list --ordered` and
    `llz env next <name>` read this to drive a sequential promote-on-green CI flow;
    Terraform itself does not consume it (it is a CI-orchestration marker that
    rides the same single-source-of-truth tfvars as ha_role).
  EOT
  type        = number
  default     = 0
  validation {
    condition     = var.promotion_rank >= 0
    error_message = "promotion_rank must be >= 0 (0 = not in a promotion pipeline)."
  }
}

variable "k8s_version" {
  description = "Enterprise LKE Kubernetes version, for example v1.31.8+lke5."
  type        = string
}

variable "tags" {
  description = "Tags applied to the cluster."
  type        = list(string)
  default     = ["platform"]
}

variable "node_pool_label" {
  description = "Label for the default node pool."
  type        = string
  default     = "platform-pool"
}

variable "node_type" {
  description = "Linode instance type for worker nodes."
  type        = string
  default     = "g8-dedicated-8-4"
}

variable "node_count" {
  description = "Number of nodes in the default node pool when autoscaling is disabled. Default 5: apl-core + the full support-plane (OpenBao HA ×3, ESO, Argo, cert-automation, firewall) does not fit on 3× g8-dedicated-8-4 — OpenBao followers go Pending on Insufficient memory / max Block Storage volume count, stalling Bootstrap OpenBao. node_count is NOT ForceNew (scales the pool live)."
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

variable "firewall_label" {
  description = "Label for the LKE node firewall. Linode labels are capped at 32 characters."
  type        = string
  default     = "platform-nodes-fw"
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
