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
  description = "Linode Object Storage endpoint host prefix (e.g. us-ord-1, us-ord-10) — the full endpoint identity, NOT just the region. Buckets are placed in region replace(obj_cluster, -N) and pinned to the endpoint whose S3 host is <obj_cluster>.linodeobjects.com (resolved to its endpoint_type: us-ord-1→E1, us-ord-10→E3), so consumers that derive their S3 host from this value reach the buckets. A region can expose several endpoints of different generations (E0/E1 legacy, E2/E3 standard) and a bucket is reachable ONLY at the one it was created against. Run `linode-cli object-storage endpoints-list` to list endpoints + types."
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

# How long a DELETED or OVERWRITTEN OpenBao snapshot stays recoverable as a
# noncurrent version in the versioned backup bucket. 30 days: four times the
# CronJob's default seven-day retention, so a snapshot retention pruned is still
# there a month later — long past the point a "we restored the wrong one" or a
# retention bug is noticed — while the bucket stays a few hundred megabytes. Lower
# it and the safety net under prune shrinks; it does not affect how many current
# snapshots exist (that is spec.components.openbaoBackup.backupKeep).
variable "openbao_backup_noncurrent_days" {
  description = "Days a deleted/overwritten OpenBao snapshot is kept as a noncurrent version in the versioned openbao-backup bucket before expiring."
  type        = number
  default     = 30

  validation {
    condition     = var.openbao_backup_noncurrent_days >= 1 && floor(var.openbao_backup_noncurrent_days) == var.openbao_backup_noncurrent_days
    error_message = "openbao_backup_noncurrent_days must be a whole number of days >= 1."
  }
}

# How long a DELETED or OVERWRITTEN Velero backup object stays recoverable as a
# noncurrent version in the versioned velero bucket. 14 days: two weeks past the
# moment Velero's GC (or an operator) deleted a backup is long enough to notice
# "the backup we needed expired" or a GC bug, while a resources-only backup set
# stays small. It does not change how long backups are KEPT — that is the
# Schedule's TTL, spec.components.velero.retention (default 30d).
variable "velero_noncurrent_days" {
  description = "Days a deleted/overwritten Velero backup object is kept as a noncurrent version in the versioned velero bucket before expiring."
  type        = number
  default     = 14

  validation {
    condition     = var.velero_noncurrent_days >= 1 && floor(var.velero_noncurrent_days) == var.velero_noncurrent_days
    error_message = "velero_noncurrent_days must be a whole number of days >= 1."
  }
}

# How long a DELETED or OVERWRITTEN CNPG backup object (a base backup or an
# archived WAL segment) stays recoverable as a noncurrent version in the versioned
# cnpg bucket. 30 days, longer than velero's 14, because this bucket's key is
# SHARED with Loki and Harbor (apl-core has one object-storage credential for every
# app — see main.tf): the window is how long a destructive write by something that
# should never touch database backups stays reversible. It does not change how
# long backups are KEPT — that is barman's retentionPolicy,
# spec.components.cnpgBackup.retention (default 30d).
variable "cnpg_noncurrent_days" {
  description = "Days a deleted/overwritten CNPG backup object is kept as a noncurrent version in the versioned cnpg bucket before expiring."
  type        = number
  default     = 30

  validation {
    condition     = var.cnpg_noncurrent_days >= 1 && floor(var.cnpg_noncurrent_days) == var.cnpg_noncurrent_days
    error_message = "cnpg_noncurrent_days must be a whole number of days >= 1."
  }
}
