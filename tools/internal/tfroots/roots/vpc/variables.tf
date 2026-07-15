variable "linode_token" {
  description = "Linode API token. Supply via TF_VAR_linode_token — do not set in tfvars."
  type        = string
  sensitive   = true
}

variable "vpc_label" {
  description = "Label of the shared VPC (the spec.networks key). Rendered by `llz render` into vpc/<name>.tfvars."
  type        = string
}

variable "region" {
  description = "Linode region for the shared VPC (the network's region; every attaching env must match it)."
  type        = string
}
