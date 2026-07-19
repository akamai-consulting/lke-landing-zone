output "firewall_id" {
  description = "Cloud Firewall ID. Pass as firewall_id in the linode_lke_node_pool resource."
  value       = linode_firewall.this.id
}

output "firewall_label" {
  description = "Resolved label of the Cloud Firewall."
  value       = linode_firewall.this.label
}
