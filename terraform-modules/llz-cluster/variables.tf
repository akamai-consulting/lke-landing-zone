# ── Cluster identity ──────────────────────────────────────────────────────────

variable "cluster_label" {
  description = "Unique label for the LKE Enterprise cluster. Also used to derive VPC, subnet, and firewall labels."
  type        = string
}

variable "region" {
  description = "Linode region for the cluster, for example us-lax or us-ord."
  type        = string
}

variable "k8s_version" {
  description = "LKE Enterprise Kubernetes version, for example v1.32.9+lke4."
  type        = string
}

variable "tags" {
  description = "Tags applied to all resources created by this module."
  type        = list(string)
  default     = []
}

# ── Networking ────────────────────────────────────────────────────────────────

variable "vpc_subnet_cidr" {
  description = "IPv4 CIDR for the VPC subnet used by LKE worker nodes. LKE-E requires /13 or /14."
  type        = string
  default     = "10.0.0.0/13"
}

variable "vpc_id" {
  description = <<-EOT
    Attach this cluster to an EXISTING (shared) VPC by ID instead of creating a
    dedicated <cluster_label>-vpc. Empty (the default) = create a dedicated VPC,
    the original behavior. When set, only this cluster's subnet is created inside
    the shared VPC; subnets across clusters sharing a VPC must not overlap.
    NOTE: multiple LKE-E clusters sharing one VPC is unverified — see the spec's
    cluster.network.vpc and the shared-VPC bootstrap-ordering note before relying on it.
  EOT
  type        = string
  default     = ""
}

# ── Control plane ─────────────────────────────────────────────────────────────

variable "control_plane_high_availability" {
  description = "Whether to enable LKE control-plane HA."
  type        = bool
  default     = true
}

variable "control_plane_audit_logs_enabled" {
  description = "Whether to enable control-plane audit logs."
  type        = bool
  default     = true
}

variable "control_plane_acl_ipv4" {
  description = "Static IPv4 CIDRs allowed to reach the LKE API server. GitHub runner CIDRs are merged in automatically when github_runner_ipv4_cidrs is set."
  type        = list(string)
  default     = []
}

variable "control_plane_acl_ipv6" {
  description = "Static IPv6 CIDRs allowed to reach the LKE API server. GitHub runner CIDRs are merged in automatically when github_runner_ipv6_cidrs is set."
  type        = list(string)
  default     = []
}

# ── Firewall ──────────────────────────────────────────────────────────────────

variable "firewall_label" {
  description = "Override the Cloud Firewall label. Defaults to '<cluster_label>-nodes' (truncated to 32 characters)."
  type        = string
  default     = ""
}

variable "github_runner_ipv4_cidrs" {
  description = "IPv4 CIDRs for GitHub Actions runners. Adds NodePort inbound rules to the node firewall and merges the CIDRs into the bootstrap control-plane ACL."
  type        = list(string)
  default     = []
}

variable "github_runner_ipv6_cidrs" {
  description = "IPv6 CIDRs for GitHub Actions runners. Adds NodePort inbound rules to the node firewall and merges the CIDRs into the bootstrap control-plane ACL."
  type        = list(string)
  default     = []
}

variable "control_plane_cidr" {
  description = "Linode private network CIDR the LKE control plane uses to reach worker nodes (kubelet, DNS, Calico, etc.)."
  type        = string
  default     = "192.168.128.0/17"
}

variable "nodebalancer_cidr" {
  description = "Linode NodeBalancer source CIDR. NodeBalancers health-check and forward traffic from this range."
  type        = string
  default     = "192.168.255.0/24"
}

variable "apl_enabled" {
  description = <<-EOT
    Enable Linode's MANAGED App Platform on this cluster (linode_lke_cluster.apl_enabled,
    v4beta/enterprise only). When true, Linode installs+manages apl-core AND provisions
    the lke<clusterID>.akamai-apl.net domain + DNS + wildcard cert. `llz ci bootstrap-cluster`
    then SKIPS its own apl-core install. Default false = LLZ self-installs apl-core (unchanged).
    ForceNew: this is fixed at cluster creation; flipping it recreates the cluster.
    See docs/adr/0005-managed-app-platform.md.
  EOT
  type        = bool
  default     = false
}
