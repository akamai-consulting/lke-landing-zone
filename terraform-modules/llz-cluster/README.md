# llz-cluster

Terraform module that provisions a secure LKE Enterprise cluster with no default node pool.

The module creates the supporting VPC and subnet, calls [`llz-node-firewall`](../llz-node-firewall/) to stamp a baseline Cloud Firewall, and then provisions the LKE-E cluster with a locked-down control-plane ACL. No node pools are created — callers attach them separately using the `node_firewall_id` output, which allows pool configuration, autoscaling, and lifecycle to be controlled independently of cluster infrastructure.

---

## Architecture

```
llz-cluster
├── linode_vpc               (dedicated VPC for the cluster)
├── linode_vpc_subnet        (worker node subnet, /13 or /14)
├── module.node_firewall     (llz-node-firewall — bootstrap baseline rules)
└── linode_lke_cluster       (tier = "enterprise", no node pool)
```

Node pools are added by the caller after the cluster is ready:

```
caller
├── module.cluster           (this module)
└── linode_lke_node_pool     (one or more, referencing module.cluster.node_firewall_id)
```

---

## Usage

### Minimal

```hcl
module "cluster" {
  source = "../../terraform-modules/llz-cluster"

  cluster_label = "platform-primary"
  region        = "us-ord"
  k8s_version   = "v1.32.9+lke4"
  tags          = ["platform", "primary"]

  vpc_subnet_cidr = "10.0.0.0/13"
}

resource "linode_lke_node_pool" "app" {
  cluster_id      = module.cluster.cluster_id
  label           = "app-pool"
  type            = "g7-premium-2"
  node_count      = 3
  firewall_id     = module.cluster.node_firewall_id
  disk_encryption = "enabled"
}
```

### With GitHub Actions runner access

Runner CIDRs are merged into the bootstrap control-plane ACL (so runners can reach `kubectl`) and add NodePort inbound rules to the node firewall (for integration tests and deployment health checks).

```hcl
module "cluster" {
  source = "../../terraform-modules/llz-cluster"

  cluster_label = "platform-primary"
  region        = "us-ord"
  k8s_version   = "v1.32.9+lke4"
  tags          = ["platform", "primary"]

  vpc_subnet_cidr          = "10.0.0.0/13"
  github_runner_ipv4_cidrs = ["4.148.0.0/16", "20.200.0.0/13"]
}
```

### Consuming the kubeconfig

The module never writes the kubeconfig to disk — take it from the output:

```
terraform output -raw kubeconfig_raw > ~/.kube/platform-primary.yaml
```

---

## Adding node pools

This module intentionally creates no node pools. Add them as separate resources in the calling configuration so that pool count, type, autoscaling, and labels can be tuned independently — and so that `terraform destroy` on the pool does not tear down the cluster.

```hcl
resource "linode_lke_node_pool" "workers" {
  cluster_id      = module.cluster.cluster_id
  label           = "workers"
  type            = "g7-premium-2"
  node_count      = 3
  firewall_id     = module.cluster.node_firewall_id
  disk_encryption = "enabled"

  labels = {
    role = "worker"
  }
}

resource "linode_lke_node_pool" "gpu" {
  cluster_id      = module.cluster.cluster_id
  label           = "gpu"
  type            = "g7-gpu-a100-2"
  node_count      = 1
  firewall_id     = module.cluster.node_firewall_id
  disk_encryption = "enabled"

  labels = {
    role = "gpu"
  }
}
```

---

## Control-plane ACL and firewall handoff

At `terraform apply` time, the module writes a bootstrap ACL to the cluster control plane (your static CIDRs plus any runner CIDRs). After that, the `cloud-firewall-controller` owns the ACL via the Linode API. Terraform ignores ACL drift on subsequent applies so it does not overwrite the controller's live state.

The node firewall follows the same model via `llz-node-firewall` — see that module's README for full details on the handoff and the `terraform state rm` escape hatch.

---

## Inputs

| Name | Type | Default | Description |
|---|---|---|---|
| `cluster_label` | `string` | required | Unique cluster label. Derived into VPC, subnet, and firewall names. |
| `region` | `string` | required | Linode region, e.g. `us-ord`, `us-sea`. |
| `k8s_version` | `string` | required | LKE-E Kubernetes version, e.g. `v1.32.9+lke4`. |
| `tags` | `list(string)` | `[]` | Tags applied to all resources. |
| `vpc_subnet_cidr` | `string` | `"10.0.0.0/13"` | Worker node subnet CIDR. LKE-E requires `/13` or `/14`. |
| `control_plane_high_availability` | `bool` | `true` | Enable control-plane HA. |
| `control_plane_audit_logs_enabled` | `bool` | `true` | Enable control-plane audit logs. |
| `control_plane_acl_ipv4` | `list(string)` | `[]` | Static IPv4 CIDRs for the bootstrap control-plane ACL. |
| `control_plane_acl_ipv6` | `list(string)` | `[]` | Static IPv6 CIDRs for the bootstrap control-plane ACL. |
| `firewall_label` | `string` | `""` | Override the Cloud Firewall label. Defaults to `<cluster_label>-nodes`. |
| `github_runner_ipv4_cidrs` | `list(string)` | `[]` | Runner IPv4 CIDRs — adds NodePort rules and merges into bootstrap ACL. |
| `github_runner_ipv6_cidrs` | `list(string)` | `[]` | Runner IPv6 CIDRs — adds NodePort rules and merges into bootstrap ACL. |

## Outputs

| Name | Description |
|---|---|
| `cluster_id` | LKE cluster ID. |
| `api_endpoints` | Kubernetes API server endpoints. |
| `kubeconfig_raw` | Decoded kubeconfig. Sensitive. |
| `vpc_id` | VPC ID. |
| `vpc_subnet_id` | Worker node subnet ID. |
| `node_firewall_id` | Cloud Firewall ID — pass as `firewall_id` on `linode_lke_node_pool`. |
| `node_firewall_label` | Resolved Cloud Firewall label. |

## Requirements

| Name | Version |
|---|---|
| Terraform | `>= 1.5.0` |
| `linode/linode` provider | `~> 3.11` |
| `hashicorp/local` provider | `~> 2.5` |
