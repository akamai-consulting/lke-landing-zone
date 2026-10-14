# ── Node firewall (bootstrap baseline) ───────────────────────────────────────
#
# Formerly the separate llz-node-firewall module. It had exactly one consumer
# (this module, via a relative source) and was never called by a root, so the
# module boundary bought indirection and nothing else — every input it took was
# already a variable here. Folded in; the rules below are unchanged.

# State migration for consumers upgrading across the inline. Module-relative, so
# this resolves to <caller>.module.cluster.module.node_firewall.linode_firewall.this
# → <caller>.module.cluster.linode_firewall.this and the firewall is NOT recreated.
moved {
  from = module.node_firewall.linode_firewall.this
  to   = linode_firewall.this
}

locals {
  # The baseline inbound rule set, as data. Every rule is ACCEPT against an
  # inbound_policy of DROP, so relative order carries no meaning — but the list
  # order is preserved on create anyway, so this stays byte-comparable with the
  # firewall Terraform used to stamp from thirteen hand-written blocks.
  #
  # EAA SSH-bastion (TCP/22) and HTTPS (TCP/443) are deliberately absent: the
  # in-cluster cloud-firewall-controller resolves those CIDRs from the Linode
  # firewall template via the API and reconciles them (with a committed
  # firewall_rules/ fallback) every cycle. Terraform lays down this static
  # baseline plus the optional GitHub-runner NodePort rules and then hands off.
  firewall_rules = concat(
    [
      # VPC intra-cluster — Cilium overlay (VXLAN 8472), health checks (4240),
      # and inter-node DNS, all sourced from the VPC subnet.
      { label = "allow-vpc-intra-tcp", protocol = "TCP", ports = "1-65535", ipv4 = [var.vpc_subnet_cidr], ipv6 = null },
      { label = "allow-vpc-intra-udp", protocol = "UDP", ports = "1-65535", ipv4 = [var.vpc_subnet_cidr], ipv6 = null },

      # ICMP, all sources.
      { label = "allow-icmp", protocol = "ICMP", ports = null, ipv4 = ["0.0.0.0/0"], ipv6 = ["::/0"] },

      # Control-plane → node, over the Linode private network.
      { label = "allow-kubelet", protocol = "TCP", ports = "10250,10256", ipv4 = [var.control_plane_cidr], ipv6 = null },
      { label = "allow-lke-wireguard", protocol = "UDP", ports = "51820", ipv4 = [var.control_plane_cidr], ipv6 = null },
      { label = "allow-cluster-dns-tcp", protocol = "TCP", ports = "53", ipv4 = [var.control_plane_cidr], ipv6 = null },
      { label = "allow-cluster-dns-udp", protocol = "UDP", ports = "53", ipv4 = [var.control_plane_cidr], ipv6 = null },
      { label = "allow-calico-bgp", protocol = "TCP", ports = "179", ipv4 = [var.control_plane_cidr], ipv6 = null },
      { label = "allow-calico-typha", protocol = "TCP", ports = "5473", ipv4 = [var.control_plane_cidr], ipv6 = null },
      { label = "allow-cluster-ipencap", protocol = "IPENCAP", ports = null, ipv4 = [var.control_plane_cidr], ipv6 = null },
      { label = "allow-prometheus-healthcheck", protocol = "TCP", ports = "9098", ipv4 = [var.control_plane_cidr], ipv6 = null },

      # NodePort services — NodeBalancer health checks and forwarded traffic.
      { label = "allow-nodeports-tcp", protocol = "TCP", ports = "30000-32767", ipv4 = [var.nodebalancer_cidr], ipv6 = null },
      { label = "allow-nodeports-udp", protocol = "UDP", ports = "30000-32767", ipv4 = [var.nodebalancer_cidr], ipv6 = null },
    ],
    # Optional: GitHub Actions runner NodePort access, for integration tests and
    # deployment health checks. The same CIDRs are concat()'d into the bootstrap
    # control-plane ACL in main.tf.
    local.has_runner_cidrs ? [
      {
        label    = "allow-nodeports-tcp-gh-runners"
        protocol = "TCP"
        ports    = "30000-32767"
        ipv4     = length(var.github_runner_ipv4_cidrs) > 0 ? var.github_runner_ipv4_cidrs : null
        ipv6     = length(var.github_runner_ipv6_cidrs) > 0 ? var.github_runner_ipv6_cidrs : null
      },
      {
        label    = "allow-nodeports-udp-gh-runners"
        protocol = "UDP"
        ports    = "30000-32767"
        ipv4     = length(var.github_runner_ipv4_cidrs) > 0 ? var.github_runner_ipv4_cidrs : null
        ipv6     = length(var.github_runner_ipv6_cidrs) > 0 ? var.github_runner_ipv6_cidrs : null
      },
    ] : [],
  )
}

resource "linode_firewall" "this" {
  #checkov:skip=CKV_LIN_6: LKE nodes need unrestricted egress for image pulls, DNS, and workload traffic.
  label           = local.firewall_label
  tags            = var.tags
  inbound_policy  = "DROP"
  outbound_policy = "ACCEPT"

  dynamic "inbound" {
    for_each = local.firewall_rules
    content {
      label    = inbound.value.label
      action   = "ACCEPT"
      protocol = inbound.value.protocol
      ports    = inbound.value.ports
      ipv4     = inbound.value.ipv4
      ipv6     = inbound.value.ipv6
    }
  }

  # This is a bootstrap baseline only. After the node pool initialises, the
  # cloud-firewall-controller and ACL controller take over rule management via
  # the Linode API. Ignoring rule drift here prevents Terraform from overwriting
  # their live state on subsequent applies. (Only `inbound` is listed: the
  # resource declares outbound_policy but no outbound blocks, so ignoring
  # `outbound` was a no-op naming a rule set that does not exist.)
  #
  # To fully remove the resource from state after handoff:
  #   terraform state rm module.<name>.linode_firewall.this
  lifecycle {
    ignore_changes = [inbound]
  }
}
