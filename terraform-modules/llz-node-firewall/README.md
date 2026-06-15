# llz-node-firewall

Terraform module that provisions a baseline Linode Cloud Firewall for LKE Enterprise node pools.

This module is intended as a **bootstrap initializer**, not a permanent fixture. It stamps a sensible default-deny inbound / permit-all outbound ruleset the first time a node pool is created. After that initial apply, the `cloud-firewall-controller` and the ACL controller own rule changes via the Linode API — Terraform will not overwrite them.

---

## Usage

```hcl
module "lke_node_firewall" {
  source = "../../terraform-modules/llz-node-firewall"

  label           = "my-cluster-nodes"
  vpc_subnet_cidr = "10.0.0.0/13"
  tags            = ["production"]
}

resource "linode_lke_node_pool" "this" {
  cluster_id  = linode_lke_cluster.this.id
  type        = "g7-premium-2"
  node_count  = 3
  firewall_id = module.lke_node_firewall.firewall_id
}
```

### With GitHub Actions runner access

When runners need to reach NodePorts (integration tests, deployment health checks) **and** the LKE-E control-plane API, pass the runner CIDRs. The module adds NodePort inbound rules to the firewall and exposes the same CIDRs as outputs for `concat()` into the cluster's control-plane ACL.

```hcl
module "lke_node_firewall" {
  source = "../../terraform-modules/llz-node-firewall"

  label                    = "my-cluster-nodes"
  vpc_subnet_cidr          = "10.0.0.0/13"
  github_runner_ipv4_cidrs = ["4.148.0.0/16", "20.200.0.0/13"]
}

resource "linode_lke_cluster" "this" {
  # ...
  control_plane {
    acl {
      enabled = true
      addresses {
        # Merge your static allow-list with the runner CIDRs from the module.
        ipv4 = concat(var.control_plane_acl_ipv4, module.lke_node_firewall.acl_cidrs_ipv4)
        ipv6 = concat(var.control_plane_acl_ipv6, module.lke_node_firewall.acl_cidrs_ipv6)
      }
    }
  }
  lifecycle {
    ignore_changes = [control_plane[0].acl]
  }
}
```

---

## Bootstrap and handoff

The firewall is created with `lifecycle { ignore_changes = [inbound, outbound] }`. This means:

- **First apply** — the full baseline ruleset is written to the Linode API.
- **Subsequent applies** — Terraform detects the resource still exists but never plans rule changes, so the `cloud-firewall-controller` and ACL controller can modify rules freely without drift conflicts.

To remove the resource from Terraform state entirely after handoff (optional, but cleanest):

```
terraform state rm module.lke_node_firewall.linode_firewall.this
```

---

## Baseline inbound rules

| Rule label | Protocol | Ports | Source | Purpose |
|---|---|---|---|---|
| `allow-vpc-intra-tcp` | TCP | 1–65535 | `vpc_subnet_cidr` | Cilium overlay, health checks, inter-node DNS |
| `allow-vpc-intra-udp` | UDP | 1–65535 | `vpc_subnet_cidr` | Same as above |
| `allow-icmp` | ICMP | — | `0.0.0.0/0`, `::/0` | Network reachability |
| `allow-kubelet` | TCP | 10250, 10256 | `control_plane_cidr` | Kubelet API |
| `allow-lke-wireguard` | UDP | 51820 | `control_plane_cidr` | LKE WireGuard tunnel |
| `allow-cluster-dns-tcp` | TCP | 53 | `control_plane_cidr` | Control-plane DNS |
| `allow-cluster-dns-udp` | UDP | 53 | `control_plane_cidr` | Control-plane DNS |
| `allow-calico-bgp` | TCP | 179 | `control_plane_cidr` | Calico BGP routing |
| `allow-calico-typha` | TCP | 5473 | `control_plane_cidr` | Calico Typha API |
| `allow-cluster-ipencap` | IPENCAP | — | `control_plane_cidr` | IP-in-IP encapsulation |
| `allow-prometheus-healthcheck` | TCP | 9098 | `control_plane_cidr` | Prometheus health endpoint |
| `allow-nodeports-tcp` | TCP | 30000–32767 | `nodebalancer_cidr` | NodeBalancer health checks + traffic |
| `allow-nodeports-udp` | UDP | 30000–32767 | `nodebalancer_cidr` | NodeBalancer health checks + traffic |
| `allow-nodeports-tcp-gh-runners` _(optional)_ | TCP | 30000–32767 | `github_runner_ipv4/ipv6_cidrs` | Runner integration-test / deployment access |
| `allow-nodeports-udp-gh-runners` _(optional)_ | UDP | 30000–32767 | `github_runner_ipv4/ipv6_cidrs` | Runner integration-test / deployment access |

Outbound default: **ACCEPT** (nodes need unrestricted egress for image pulls, DNS, and workload traffic).

---

## Inputs

| Name | Type | Default | Description |
|---|---|---|---|
| `label` | `string` | required | Cloud Firewall label. Linode enforces a 32-character maximum. |
| `vpc_subnet_cidr` | `string` | required | VPC subnet CIDR for LKE worker nodes. LKE-E requires `/13` or `/14`. |
| `tags` | `list(string)` | `[]` | Tags applied to the firewall resource. |
| `control_plane_cidr` | `string` | `"192.168.128.0/17"` | Linode private network CIDR used by the control plane to reach worker nodes. |
| `nodebalancer_cidr` | `string` | `"192.168.255.0/24"` | Linode NodeBalancer source CIDR. |
| `github_runner_ipv4_cidrs` | `list(string)` | `[]` | IPv4 CIDRs for GitHub Actions runners. Activates optional NodePort rules and ACL outputs. |
| `github_runner_ipv6_cidrs` | `list(string)` | `[]` | IPv6 CIDRs for GitHub Actions runners. Activates optional NodePort rules and ACL outputs. |

## Outputs

| Name | Description |
|---|---|
| `firewall_id` | Cloud Firewall ID. Pass as `firewall_id` on `linode_lke_node_pool`. |
| `firewall_label` | Resolved label of the Cloud Firewall. |
| `acl_cidrs_ipv4` | GitHub runner IPv4 CIDRs for `concat()` into the LKE-E control-plane ACL. |
| `acl_cidrs_ipv6` | GitHub runner IPv6 CIDRs for `concat()` into the LKE-E control-plane ACL. |

## Requirements

| Name | Version |
|---|---|
| Terraform | `>= 1.5.0` |
| `linode/linode` provider | `~> 3.11` |
