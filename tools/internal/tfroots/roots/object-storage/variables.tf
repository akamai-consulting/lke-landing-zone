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

