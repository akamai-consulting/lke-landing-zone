variable "region_suffix" {
  description = "Deployment suffix appended to bucket and key labels — the lowercase deployment/env name (e.g. primary, secondary, staging, lab, e2e, or an adopter's own env). Despite the variable name, this is not strictly a geographic region: it is the deployment discriminator and must match the cluster workspace deployment (the platform's own pairing is primary → us-ord, secondary → us-sea, staging → us-ord, lab → us-ord). Environments are created dynamically by template-scripts/new-deployment.sh, so this is validated by format, not a fixed list."
  type        = string
  # Format validation (and the your-env placeholder rejection) lives in
  # llz-object-storage, which this root passes the value through to verbatim —
  # it is the module's published contract, and duplicating it here only meant
  # two copies of one regex to keep in sync.
}

variable "obj_cluster" {
  description = "Linode Object Storage cluster identifier for bucket placement (e.g. us-ord-1, us-sea-1). Run `linode-cli object-storage clusters-list` to list available clusters."
  type        = string
}

# (obj_key_rotation_days was REMOVED with the TF-managed access keys — the
# in-cluster linodeCredRotator CronJob owns rotation; first keys are minted by
# `llz ci mint-bootstrap-objkeys` at bootstrap.)


# The per-instance namespace on every bucket label. Linode Object Storage bucket
# labels share ONE namespace per region ACROSS ACCOUNTS, so a shared prefix means
# the first instance to use a given deployment name in a region takes the name
# globally and every later one fails its apply with "[400] ... already exists".
# This root used to leave label_prefix at the module default ("platform") and
# declared no variable for it, so there was no way to set it at all.
#
# No default: `llz render` always emits it from spec.instance.objLabelPrefix
# (tools/internal/clusterspec/objlabels.go), and a missing value must fail at plan
# rather than silently recreating the collision this variable exists to remove.
variable "label_prefix" {
  description = "Per-instance prefix for bucket and key labels (spec.instance.objLabelPrefix). Bucket labels become <label_prefix>-loki-chunks-<region_suffix>, etc."
  type        = string

  validation {
    # Rejects the SHIPPED PLACEHOLDER as well as the grammar, mirroring
    # region_suffix's `!= "your-env"`. Without it two adopters hand-authoring
    # tfvars from the example both create `your-instance-…` — the identical
    # collision this variable exists to remove, under a new shared name.
    condition     = can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$", var.label_prefix)) && var.label_prefix != "your-instance"
    error_message = "label_prefix must be lowercase alphanumerics and hyphens, not starting or ending with a hyphen (Linode bucket labels are S3-shaped), and must not be the shipped placeholder \"your-instance\" — `llz render` fills it from spec.instance.objLabelPrefix."
  }
}
