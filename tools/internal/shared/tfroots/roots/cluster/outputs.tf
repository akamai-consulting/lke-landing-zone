output "cluster_id" {
  description = "The LKE cluster ID."
  value       = module.cluster.cluster_id
}

output "api_endpoints" {
  description = "Kubernetes API endpoints for the cluster."
  value       = module.cluster.api_endpoints
}

output "kubeconfig_raw" {
  description = "Raw kubeconfig content. Read from state by CI — `llz ci fetch-kubeconfig-state`, and `llz ci tf-output kubeconfig_raw --out-file` in the rotation lane. Never written to disk by Terraform (the cluster-bootstrap workspace that formerly consumed it via terraform_remote_state is gone). Marked sensitive."
  value       = module.cluster.kubeconfig_raw
  sensitive   = true
}

output "vpc_id" {
  description = "The ID of the VPC wrapping the cluster."
  value       = module.cluster.vpc_id
}

# The module has exposed vpc_subnet_id since it was written; the root never
# re-exported it, which was harmless while nothing outside the cluster attached to
# the subnet. The `databases` root changed that: spec.cluster.databases.<name>
# REQUIRES subnetId, and without this output the only way to fill it is
# `linode-cli vpcs subnets-list <vpc_id>` — an out-of-band lookup for a value
# Terraform already holds, which is how a spec ends up with a stale or wrong id.
# Pairs with vpc_id above: `terraform output vpc_id`/`vpc_subnet_id` in the
# cluster workspace gives both fields the databases spec needs.
output "vpc_subnet_id" {
  description = "The ID of the VPC subnet the cluster's nodes attach to. Fills spec.cluster.databases.<name>.subnetId — a Managed Database attaches to a subnet of the VPC in vpc_id."
  value       = module.cluster.vpc_subnet_id
}

output "node_firewall_id" {
  description = "The ID of the Linode Cloud Firewall attached to the LKE node pool."
  value       = module.cluster.node_firewall_id
}

output "node_firewall_label" {
  description = "The label of the Linode Cloud Firewall attached to the LKE node pool. Used by the teardown's force-delete fallback to find the firewall by name when it can't be resolved by id (var.firewall_label defaults to \"platform-nodes-fw\", which a \"<cluster_label>-nodes\" reconstruction would miss)."
  value       = module.cluster.node_firewall_label
}
