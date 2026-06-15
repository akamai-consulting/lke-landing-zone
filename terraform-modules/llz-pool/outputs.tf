output "node_pool_id" {
  description = "ID of the node pool."
  value       = linode_lke_node_pool.this.id
}

output "node_pool_label" {
  description = "Label of the node pool."
  value       = linode_lke_node_pool.this.label
}

output "nodes" {
  description = "List of node objects in the pool, each containing id, instance_id, and status."
  value       = linode_lke_node_pool.this.nodes
}
