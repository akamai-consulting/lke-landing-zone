# ── Cluster ───────────────────────────────────────────────────────────────────

output "cluster_id" {
  description = "LKE cluster ID."
  value       = linode_lke_cluster.this.id
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

# ── Networking ────────────────────────────────────────────────────────────────

# THESE REPORT WHERE THE CLUSTER ACTUALLY IS, NOT WHAT THIS MODULE BUILT.
#
# They used to return local.vpc_id / linode_vpc_subnet.nodes.id — the VPC and
# subnet created here. On every cluster those agree, EXCEPT one: a cluster
# created before this module passed vpc_id got its own LKE-E-provisioned VPC and
# left the module's orphaned (see the check block in main.tf). There, the old
# outputs named a VPC with no nodes in it.
#
# That is not a cosmetic difference, because of who reads these. The cluster
# root re-exports both, and the description below is an instruction operators
# follow: a Managed Postgres is VPC-only, with no public endpoint, so a wrong
# vpc_id/subnet_id here produces a database the cluster cannot reach — and the
# failure surfaces as a connection timeout from a pod, nowhere near this file.
# Reading the attributes back off the cluster makes the answer true in both
# cases: identical on a healthy cluster, correct on a drifted one.
output "vpc_id" {
  description = "ID of the VPC the cluster's nodes are actually attached to (normally the dedicated one created here, or the shared vpc_id passed in)."
  value       = tostring(linode_lke_cluster.this.vpc_id)
}

output "vpc_subnet_id" {
  description = "ID of the VPC subnet the cluster's LKE worker nodes are actually attached to."
  value       = tostring(linode_lke_cluster.this.subnet_id)
}

# DELIBERATELY the declared value, not a live read — the asymmetry with the two
# outputs above is the point. This is the CIDR the node firewall's intra-VPC
# rules were actually built from (firewall.tf reads the same variable), so
# returning a live-looking value here would hide a mismatch rather than expose
# it: the firewall would still be wrong, and the output would stop saying so.
# The check block in main.tf is what reports the case where they diverge.
output "vpc_subnet_cidr" {
  description = "IPv4 CIDR of the VPC subnet (the single source of truth for all intra-cluster traffic: node, pod, and service ranges). The firewall-controller's VPC_CIDR is patched from this so its node-firewall + control-plane-ACL rules match the VPC the TF node-firewall was built from."
  value       = var.vpc_subnet_cidr
}

# ── Firewall ──────────────────────────────────────────────────────────────────

output "node_firewall_id" {
  description = "Cloud Firewall ID. Pass as firewall_id when creating linode_lke_node_pool resources."
  value       = linode_firewall.this.id
}

output "node_firewall_label" {
  description = "Resolved label of the Cloud Firewall."
  value       = linode_firewall.this.label
}
