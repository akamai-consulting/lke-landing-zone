variable "vpc_label" {
  description = "Label of the shared VPC (the spec.networks key). Rendered by `llz render` into vpc/<name>.tfvars."
  type        = string
}

variable "region" {
  description = "Linode region for the shared VPC (the network's region; every attaching env must match it)."
  type        = string
}
