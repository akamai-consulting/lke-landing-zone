output "firewall_id" {
  description = "Cloud Firewall ID. Pass as firewall_id in the linode_lke_node_pool resource."
  value       = linode_firewall.this.id
}

output "firewall_label" {
  description = "Resolved label of the Cloud Firewall."
  value       = linode_firewall.this.label
}

output "acl_cidrs_ipv4" {
  description = "GitHub runner IPv4 CIDRs. concat() these into control_plane.acl.addresses.ipv4 on the LKE-E cluster resource to grant runners API-server access at bootstrap time."
  value       = var.github_runner_ipv4_cidrs
}

output "acl_cidrs_ipv6" {
  description = "GitHub runner IPv6 CIDRs. concat() these into control_plane.acl.addresses.ipv6 on the LKE-E cluster resource to grant runners API-server access at bootstrap time."
  value       = var.github_runner_ipv6_cidrs
}
