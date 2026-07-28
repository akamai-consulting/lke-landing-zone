variable "region_suffix" {
  description = "Deployment suffix appended to the database label — the lowercase deployment/env name (e.g. primary, secondary, staging, lab, e2e). The deployment discriminator; must match the cluster workspace deployment. Format validation lives in the llz-databases module this root passes the value through to."
  type        = string
}

variable "region" {
  description = "Linode region the Managed Postgres is provisioned in (e.g. us-ord). Must match the region of the VPC in var.vpc_id."
  type        = string
}

variable "vpc_id" {
  description = "ID of the VPC to attach the database to (VPC-only, no public endpoint). Typically the same VPC the LKE cluster attaches to."
  type        = number
}

variable "subnet_id" {
  description = "ID of the VPC subnet (in var.vpc_id) to attach the database to."
  type        = number
}

variable "engine_version" {
  description = "Major PostgreSQL engine version (e.g. \"16\")."
  type        = string
  default     = "16"
}

variable "db_type" {
  description = "Linode Managed Database node type (e.g. g6-dedicated-2). Run `linode-cli databases types` to list."
  type        = string
  default     = "g6-dedicated-2"
}

variable "cluster_size" {
  description = "Number of nodes: 1 (single) or 2/3 (HA). Production defaults to 2."
  type        = number
  default     = 2
}
