resource "linode_lke_node_pool" "this" {
  cluster_id = var.cluster_id
  label      = var.label
  type       = var.node_type
  tags       = var.tags

  # Static count is ignored by the API when autoscaling is enabled.
  node_count = var.autoscaler_enabled ? null : var.node_count

  # Security invariants — not configurable.
  firewall_id     = var.node_firewall_id
  disk_encryption = "enabled"

  labels = var.node_labels

  dynamic "taint" {
    for_each = var.node_taints
    content {
      key    = taint.value.key
      value  = taint.value.value
      effect = taint.value.effect
    }
  }

  dynamic "autoscaler" {
    for_each = var.autoscaler_enabled ? [1] : []
    content {
      min = var.autoscaler_min
      max = var.autoscaler_max
    }
  }
}
