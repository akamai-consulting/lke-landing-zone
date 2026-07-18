output "cluster_id" {
  description = "The LKE cluster ID."
  value       = module.cluster.cluster_id
}

output "ha_role" {
  description = "OpenBao HA role of this deployment (active | standby | standalone). Read by cluster-bootstrap via remote state to identify this deployment's HA role on destroy."
  value       = var.ha_role
}

output "ha_group" {
  description = "OpenBao HA group id (empty for standalone). The two deployments sharing it are peers."
  value       = var.ha_group
}

output "cluster_status" {
  description = "The LKE cluster status."
  value       = module.cluster.cluster_status
}

output "api_endpoints" {
  description = "Kubernetes API endpoints for the cluster."
  value       = module.cluster.api_endpoints
}

output "kubeconfig_raw" {
  description = "Raw kubeconfig content. Read from state by `llz ci fetch-kubeconfig-state` (the cluster-bootstrap workspace that formerly consumed it via terraform_remote_state is gone). Marked sensitive."
  value       = module.cluster.kubeconfig_raw
  sensitive   = true
}

output "vpc_id" {
  description = "The ID of the VPC wrapping the cluster."
  value       = module.cluster.vpc_id
}

output "vpc_subnet_id" {
  description = "The ID of the VPC subnet used by LKE worker nodes."
  value       = module.cluster.vpc_subnet_id
}

output "vpc_subnet_cidr" {
  description = "The IPv4 CIDR of the VPC subnet. Reference/diagnostic output: the firewall-controller's VPC_CIDR is self-discovered in-cluster by the cidrFirewall component's discover CronJob (from the node's VPC interface, the same subnet this reports); the manual-fallback `llz ci bootstrap-cloud-firewall --region` reads the CIDR from tfvars."
  value       = module.cluster.vpc_subnet_cidr
}

output "node_firewall_id" {
  description = "The ID of the Linode Cloud Firewall attached to the LKE node pool."
  value       = module.cluster.node_firewall_id
}

output "node_firewall_label" {
  description = "The label of the Linode Cloud Firewall attached to the LKE node pool. Used by the teardown's force-delete fallback to find the firewall by name when it can't be resolved by id (var.firewall_label defaults to \"platform-nodes-fw\", which a \"<cluster_label>-nodes\" reconstruction would miss)."
  value       = module.cluster.node_firewall_label
}
