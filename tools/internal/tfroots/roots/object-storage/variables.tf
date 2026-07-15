variable "linode_token" {
  description = "Linode API token. Supply via TF_VAR_linode_token — do not set in tfvars."
  type        = string
  sensitive   = true
}

variable "region_suffix" {
  description = "Deployment suffix appended to bucket and key labels — the lowercase deployment/env name (e.g. primary, secondary, staging, lab, e2e, or an adopter's own env). Despite the variable name, this is not strictly a geographic region: it is the deployment discriminator and must match the cluster workspace deployment (the platform's own pairing is primary → us-ord, secondary → us-sea, staging → us-ord, lab → us-ord). Environments are created dynamically by template-scripts/new-deployment.sh, so this is validated by format, not a fixed list."
  type        = string

  validation {
    # Match the env-name format new-deployment.sh enforces; reject the shipped
    # placeholder so an unscaffolded tfvars fails loudly instead of mislabeling
    # buckets. Not a fixed list — adopters create their own envs.
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.region_suffix)) && var.region_suffix != "your-env"
    error_message = "region_suffix must be the lowercase deployment name matching the cluster workspace (^[a-z][a-z0-9-]{1,30}$) — e.g. primary, secondary, staging, lab, e2e, or your own env — not the 'your-env' placeholder."
  }
}

variable "obj_cluster" {
  description = "Linode Object Storage cluster identifier for bucket placement (e.g. us-ord-1, us-sea-1). Run `linode-cli object-storage clusters-list` to list available clusters."
  type        = string
}

# (obj_key_rotation_days was REMOVED with the TF-managed access keys — the
# in-cluster linodeCredRotator CronJob owns rotation; first keys are minted by
# `llz ci mint-bootstrap-objkeys` at bootstrap.)

