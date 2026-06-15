locals {
  has_runner_cidrs = length(var.github_runner_ipv4_cidrs) > 0 || length(var.github_runner_ipv6_cidrs) > 0
}

resource "linode_firewall" "this" {
  #checkov:skip=CKV_LIN_6: LKE nodes need unrestricted egress for image pulls, DNS, and workload traffic.
  label           = var.label
  tags            = var.tags
  inbound_policy  = "DROP"
  outbound_policy = "ACCEPT"

  # ── VPC intra-cluster ──────────────────────────────────────────────────────
  # Covers Cilium overlay (VXLAN 8472), health checks (4240), and inter-node
  # DNS — all sourced from the VPC subnet.
  inbound {
    label    = "allow-vpc-intra-tcp"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "1-65535"
    ipv4     = [var.vpc_subnet_cidr]
  }

  inbound {
    label    = "allow-vpc-intra-udp"
    action   = "ACCEPT"
    protocol = "UDP"
    ports    = "1-65535"
    ipv4     = [var.vpc_subnet_cidr]
  }

  # ── ICMP (all sources) ─────────────────────────────────────────────────────
  inbound {
    label    = "allow-icmp"
    action   = "ACCEPT"
    protocol = "ICMP"
    ipv4     = ["0.0.0.0/0"]
    ipv6     = ["::/0"]
  }

  # ── Control-plane → node (Linode private network) ─────────────────────────
  inbound {
    label    = "allow-kubelet"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "10250,10256"
    ipv4     = [var.control_plane_cidr]
  }

  inbound {
    label    = "allow-lke-wireguard"
    action   = "ACCEPT"
    protocol = "UDP"
    ports    = "51820"
    ipv4     = [var.control_plane_cidr]
  }

  inbound {
    label    = "allow-cluster-dns-tcp"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "53"
    ipv4     = [var.control_plane_cidr]
  }

  inbound {
    label    = "allow-cluster-dns-udp"
    action   = "ACCEPT"
    protocol = "UDP"
    ports    = "53"
    ipv4     = [var.control_plane_cidr]
  }

  inbound {
    label    = "allow-calico-bgp"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "179"
    ipv4     = [var.control_plane_cidr]
  }

  inbound {
    label    = "allow-calico-typha"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "5473"
    ipv4     = [var.control_plane_cidr]
  }

  inbound {
    label    = "allow-cluster-ipencap"
    action   = "ACCEPT"
    protocol = "IPENCAP"
    ipv4     = [var.control_plane_cidr]
  }

  inbound {
    label    = "allow-prometheus-healthcheck"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "9098"
    ipv4     = [var.control_plane_cidr]
  }

  # ── NodePort services (NodeBalancer health checks + external traffic) ──────
  inbound {
    label    = "allow-nodeports-tcp"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "30000-32767"
    ipv4     = [var.nodebalancer_cidr]
  }

  inbound {
    label    = "allow-nodeports-udp"
    action   = "ACCEPT"
    protocol = "UDP"
    ports    = "30000-32767"
    ipv4     = [var.nodebalancer_cidr]
  }

  # EAA SSH-bastion (TCP/22) and HTTPS (TCP/443) rules are no longer seeded here:
  # the in-cluster cloud-firewall-controller resolves those CIDRs from the Linode
  # firewall template via the Linode API and reconciles them (with a committed
  # firewall_rules/ fallback) on every cycle. Terraform only lays down the static
  # baseline below plus the optional GitHub-runner NodePort rules.

  # ── Optional: GitHub Actions runner NodePort access ────────────────────────
  # Enables integration-test and deployment health-check traffic from runners.
  # Set github_runner_ipv4_cidrs / github_runner_ipv6_cidrs to activate.
  # The same CIDRs are exposed as acl_cidrs_ipv4 / acl_cidrs_ipv6 outputs for
  # concat() into the LKE-E control-plane ACL.
  dynamic "inbound" {
    for_each = local.has_runner_cidrs ? [1] : []
    content {
      label    = "allow-nodeports-tcp-gh-runners"
      action   = "ACCEPT"
      protocol = "TCP"
      ports    = "30000-32767"
      ipv4     = length(var.github_runner_ipv4_cidrs) > 0 ? var.github_runner_ipv4_cidrs : null
      ipv6     = length(var.github_runner_ipv6_cidrs) > 0 ? var.github_runner_ipv6_cidrs : null
    }
  }

  dynamic "inbound" {
    for_each = local.has_runner_cidrs ? [1] : []
    content {
      label    = "allow-nodeports-udp-gh-runners"
      action   = "ACCEPT"
      protocol = "UDP"
      ports    = "30000-32767"
      ipv4     = length(var.github_runner_ipv4_cidrs) > 0 ? var.github_runner_ipv4_cidrs : null
      ipv6     = length(var.github_runner_ipv6_cidrs) > 0 ? var.github_runner_ipv6_cidrs : null
    }
  }

  # This module seeds a bootstrap baseline only. After the node pool
  # initialises, the cloud-firewall-controller and ACL controller take over
  # rule management via the Linode API. Ignoring rule drift here prevents
  # Terraform from overwriting their live state on subsequent applies.
  #
  # To fully remove the resource from state after handoff:
  #   terraform state rm module.<name>.linode_firewall.this
  lifecycle {
    ignore_changes = [inbound, outbound]
  }
}
