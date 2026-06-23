# ── Cluster ───────────────────────────────────────────────────────────────────

output "cluster_id" {
  description = "LKE cluster ID."
  value       = linode_lke_cluster.this.id
}

output "cluster_status" {
  description = "LKE cluster status."
  value       = linode_lke_cluster.this.status
}

output "api_endpoints" {
  description = "Kubernetes API server endpoints."
  value       = linode_lke_cluster.this.api_endpoints
}

# ── Kubeconfig ────────────────────────────────────────────────────────────────

output "kubeconfig_raw" {
  description = "Decoded kubeconfig content. Marked sensitive — consume via 'terraform output -raw kubeconfig_raw'."
  value       = base64decode(linode_lke_cluster.this.kubeconfig)
  sensitive   = true
}

output "kubeconfig_path" {
  description = "Path of the kubeconfig file written to disk. Empty string when kubeconfig_path was not set."
  value       = var.kubeconfig_path != "" ? local_sensitive_file.kubeconfig[0].filename : ""
  sensitive   = true
}

# ── Networking ────────────────────────────────────────────────────────────────

output "vpc_id" {
  description = "ID of the VPC wrapping the cluster (the dedicated one created here, or the shared vpc_id passed in)."
  value       = local.vpc_id
}

output "vpc_subnet_id" {
  description = "ID of the VPC subnet used by LKE worker nodes."
  value       = linode_vpc_subnet.nodes.id
}

output "vpc_subnet_cidr" {
  description = "IPv4 CIDR of the VPC subnet (the single source of truth for all intra-cluster traffic: node, pod, and service ranges). The firewall-controller's VPC_CIDR is patched from this so its node-firewall + control-plane-ACL rules match the VPC the TF node-firewall was built from."
  value       = var.vpc_subnet_cidr
}

# ── Firewall ──────────────────────────────────────────────────────────────────

output "node_firewall_id" {
  description = "Cloud Firewall ID. Pass as firewall_id when creating linode_lke_node_pool resources."
  value       = module.node_firewall.firewall_id
}

output "node_firewall_label" {
  description = "Resolved label of the Cloud Firewall."
  value       = module.node_firewall.firewall_label
}

output "acl_cidrs_ipv4" {
  description = "GitHub runner IPv4 CIDRs surfaced from the firewall module. Useful for referencing the bootstrap ACL set in downstream resources."
  value       = module.node_firewall.acl_cidrs_ipv4
}

output "acl_cidrs_ipv6" {
  description = "GitHub runner IPv6 CIDRs surfaced from the firewall module. Useful for referencing the bootstrap ACL set in downstream resources."
  value       = module.node_firewall.acl_cidrs_ipv6
}
