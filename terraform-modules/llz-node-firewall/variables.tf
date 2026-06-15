variable "label" {
  description = "Label for the Linode Cloud Firewall. Linode labels are capped at 32 characters."
  type        = string
}

variable "vpc_subnet_cidr" {
  description = "VPC subnet CIDR used by LKE worker nodes. All intra-cluster traffic originates from this range."
  type        = string
}

variable "tags" {
  description = "Tags to apply to the firewall resource."
  type        = list(string)
  default     = []
}

variable "control_plane_cidr" {
  description = "Linode private network CIDR the LKE control plane uses to reach worker nodes (kubelet, DNS, Calico, etc.)."
  type        = string
  default     = "192.168.128.0/17"
}

variable "nodebalancer_cidr" {
  description = "Linode NodeBalancer source CIDR. NodeBalancers health-check and forward traffic from this range."
  type        = string
  default     = "192.168.255.0/24"
}

variable "github_runner_ipv4_cidrs" {
  description = "IPv4 CIDRs for GitHub Actions runners. When non-empty, NodePort inbound rules are added for these ranges. The CIDRs are also exposed via acl_cidrs_ipv4 for injection into the LKE-E control-plane ACL."
  type        = list(string)
  default     = []
}

variable "github_runner_ipv6_cidrs" {
  description = "IPv6 CIDRs for GitHub Actions runners. When non-empty, NodePort inbound rules are added for these ranges. The CIDRs are also exposed via acl_cidrs_ipv6 for injection into the LKE-E control-plane ACL."
  type        = list(string)
  default     = []
}
