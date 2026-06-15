# llz-pool

Terraform module that attaches a node pool to an LKE Enterprise cluster with security invariants enforced by the module signature.

`disk_encryption` is always `"enabled"` and `firewall_id` is a required input — both are non-negotiable and cannot be accidentally omitted or overridden. Designed to be used alongside [`llz-cluster`](../llz-cluster/), whose outputs feed directly into this module's required inputs.

---

## Usage

```hcl
module "cluster" {
  source = "../../terraform-modules/llz-cluster"

  cluster_label   = "platform-primary"
  region          = "us-ord"
  k8s_version     = "v1.32.9+lke4"
  vpc_subnet_cidr = "10.0.0.0/13"
}

module "pool" {
  source = "../../terraform-modules/llz-pool"

  cluster_id       = module.cluster.cluster_id
  node_firewall_id = module.cluster.node_firewall_id

  label     = "platform-pool"
  node_type = "g7-premium-2"
  node_count = 3

  node_labels = {
    environment = "shared"
    role        = "observability"
  }
}
```

### Multiple pools

Each pool is an independent module call. All pools on the same cluster share the same `cluster_id` and `node_firewall_id`.

```hcl
module "workers" {
  source = "../../terraform-modules/llz-pool"

  cluster_id       = module.cluster.cluster_id
  node_firewall_id = module.cluster.node_firewall_id

  label     = "workers"
  node_type = "g7-premium-2"
  node_count = 3

  node_labels = { role = "worker" }
}

module "gpu" {
  source = "../../terraform-modules/llz-pool"

  cluster_id       = module.cluster.cluster_id
  node_firewall_id = module.cluster.node_firewall_id

  label     = "gpu"
  node_type = "g7-gpu-a100-2"
  node_count = 1

  node_labels = { role = "gpu" }
  node_taints = [{
    key    = "nvidia.com/gpu"
    value  = "true"
    effect = "NoSchedule"
  }]
}
```

### With autoscaling

```hcl
module "pool" {
  source = "../../terraform-modules/llz-pool"

  cluster_id       = module.cluster.cluster_id
  node_firewall_id = module.cluster.node_firewall_id

  label     = "platform-pool"
  node_type = "g7-premium-2"

  autoscaler_enabled = true
  autoscaler_min     = 3
  autoscaler_max     = 6
}
```

---

## Inputs

| Name | Type | Default | Description |
|---|---|---|---|
| `cluster_id` | `number` | required | LKE cluster ID. Use `module.cluster.cluster_id`. |
| `node_firewall_id` | `number` | required | Cloud Firewall ID. Use `module.cluster.node_firewall_id`. |
| `label` | `string` | required | Node pool label. |
| `node_type` | `string` | required | Linode instance type, e.g. `g7-premium-2`. |
| `tags` | `list(string)` | `[]` | Tags applied to the node pool. |
| `node_labels` | `map(string)` | `{}` | Kubernetes node labels applied to all nodes. |
| `node_taints` | `list(object)` | `[]` | Kubernetes taints applied to all nodes. Each object requires `key`, `value`, and `effect`. |
| `node_count` | `number` | `3` | Static node count. Ignored when `autoscaler_enabled` is true. |
| `autoscaler_enabled` | `bool` | `false` | Enable autoscaling for this pool. |
| `autoscaler_min` | `number` | `3` | Minimum node count when autoscaling is enabled. |
| `autoscaler_max` | `number` | `6` | Maximum node count when autoscaling is enabled. |

## Outputs

| Name | Description |
|---|---|
| `node_pool_id` | Node pool ID. |
| `node_pool_label` | Node pool label. |
| `nodes` | List of node objects (`id`, `instance_id`, `status`). |

## Requirements

| Name | Version |
|---|---|
| Terraform | `>= 1.5.0` |
| `linode/linode` provider | `~> 3.11` |
