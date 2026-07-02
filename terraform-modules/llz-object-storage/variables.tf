variable "region_suffix" {
  description = "Deployment suffix appended to bucket and key labels — the lowercase deployment/env name (e.g. primary, secondary, staging, lab, e2e, or an adopter's own env). Despite the variable name, this is not strictly a geographic region: it is the deployment discriminator and must match the cluster workspace deployment (the platform's own pairing is primary → us-ord, secondary → us-sea, staging → us-ord, lab → us-ord). Environments are created dynamically, so this is validated by format, not a fixed list."
  type        = string

  validation {
    # Match the env-name format template-scripts/new-deployment.sh enforces; reject the
    # shipped placeholder so an unscaffolded tfvars fails loudly. Not a fixed
    # list — adopters create their own envs.
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.region_suffix)) && var.region_suffix != "your-env"
    error_message = "region_suffix must be the lowercase deployment name matching the cluster workspace (^[a-z][a-z0-9-]{1,30}$) — e.g. primary, secondary, staging, lab, e2e, or your own env — not the 'your-env' placeholder."
  }
}

variable "obj_cluster" {
  description = "Linode Object Storage cluster identifier for bucket placement (e.g. us-ord-1, us-sea-1). Run `linode-cli object-storage clusters-list` to list available clusters."
  type        = string
}

# (obj_key_rotation_days was REMOVED with the TF-managed access keys: key
# rotation is owned by the in-cluster linodeCredRotator CronJob — its
# rotate-after-days knob — and the first keys are minted at bootstrap by
# `llz ci mint-bootstrap-objkeys`. See main.tf's "Access keys" note.)

# Org/deployment identity, variabilized per the templatization plan (§8 / §11).
# Default is the in-repo deployment's prefix; a sibling system team overrides it
# so two deployments don't collide on bucket labels (OBJ bucket names are global
# per cluster). Bucket labels become "<label_prefix>-harbor-registry-<suffix>",
# "<label_prefix>-loki-chunks-<suffix>", etc.
variable "label_prefix" {
  description = "Prefix for all bucket and key labels. Org/deployment identity — override per sibling deployment so labels don't collide."
  type        = string
  default     = "platform"
}
