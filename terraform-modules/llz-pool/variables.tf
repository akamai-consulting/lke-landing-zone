# ── Required (from llz-cluster outputs) ────────────────────

variable "cluster_id" {
  description = "LKE cluster ID to attach this pool to. Use module.cluster.cluster_id."
  type        = number
}

variable "node_firewall_id" {
  description = "Cloud Firewall ID to attach to every node in the pool. Use module.cluster.node_firewall_id. Disk encryption is always enabled; omitting the firewall is not allowed."
  type        = number
}

# ── Pool identity ─────────────────────────────────────────────────────────────

variable "label" {
  description = "Label for the node pool."
  type        = string
}

variable "node_type" {
  description = "Linode instance type for worker nodes, for example g8-dedicated-8-4."
  type        = string
}

variable "tags" {
  description = "Tags applied to the node pool."
  type        = list(string)
  default     = []
}

variable "node_labels" {
  description = "Kubernetes node labels applied to every node in the pool."
  type        = map(string)
  default     = {}
}

variable "node_taints" {
  description = "Kubernetes taints applied to every node in the pool."
  type = list(object({
    key    = string
    value  = string
    effect = string
  }))
  default = []
}

# ── Sizing ────────────────────────────────────────────────────────────────────

variable "node_count" {
  description = "Static node count. Used when autoscaler_enabled is false."
  type        = number
  default     = 3
}

variable "autoscaler_enabled" {
  description = "Whether to enable autoscaling for this pool."
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
