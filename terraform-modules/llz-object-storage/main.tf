# ── Harbor registry object-storage bucket ─────────────────────────────────────
# Replaces the previous in-cluster 100Gi `harbor-registry` block PVC with an
# S3-backed registry. The Harbor chart's registry component is wired (via
# apl-values/<env>/values.yaml apps.harbor._rawValues) to set
# REGISTRY_STORAGE_S3_ACCESSKEY / REGISTRY_STORAGE_S3_SECRETKEY env vars
# from the harbor-registry-s3 K8s Secret synced by ESO from
# secret/harbor/registry-s3 in OpenBao.
#
# Rationale: the block volume had a hard 10Gi Linode minimum and grew
# monotonically as images accumulated. OBJ is cheaper per-GB, uncapped, and
# removes one volume from each cluster's attached-disk count — relevant to
# the LKE account-quota orphan problem documented in the
# project-lke-rebuild-orphans-quota memory.
# NOTE: an earlier commit added `force_destroy = true` here to auto-empty
# buckets before `terraform destroy` — the Linode provider's
# linode_object_storage_bucket resource does NOT support that argument
# (validated against the deployed provider version on TF run 2994938).
# Removed. The "Drain S3 buckets before destroy" step in
# .github/workflows/terraform.yml (destroy-object-storage job) now uses
# `aws s3 rm --recursive` with the bucket-scoped keys still in TF state
# to empty the buckets BEFORE `terraform destroy` runs against them.
# Track upstream linode/terraform-provider-linode for a native
# force_destroy attribute; if it lands, replace the drain step + this
# comment with the native flag.

# The Linode API now REJECTS the deprecated `cluster` argument on bucket
# creation ([400] cluster is not valid) — buckets must be placed by `region`,
# which is the cluster id with the trailing `-N` ordinal stripped (the
# provider's own deprecation notice gives the mapping: cluster `us-mia-1` →
# region `us-mia`). var.obj_cluster stays the canonical input because the S3
# endpoint hostname still uses the full cluster id — see the s3_endpoint output.
locals {
  obj_region = replace(var.obj_cluster, "/-[0-9]+$/", "")
}

resource "linode_object_storage_bucket" "harbor_registry" {
  region = local.obj_region
  label  = "${var.label_prefix}-harbor-registry-${var.region_suffix}"
}

# ── Loki object-storage buckets ───────────────────────────────────────────────
# Three buckets per region matching the names in loki-values.yaml (and the
# secondary override in loki-values-secondary.yaml). All buckets are private.

resource "linode_object_storage_bucket" "loki_chunks" {
  region = local.obj_region
  label  = "${var.label_prefix}-loki-chunks-${var.region_suffix}"
}

resource "linode_object_storage_bucket" "loki_ruler" {
  region = local.obj_region
  label  = "${var.label_prefix}-loki-ruler-${var.region_suffix}"
}

resource "linode_object_storage_bucket" "loki_admin" {
  region = local.obj_region
  label  = "${var.label_prefix}-loki-admin-${var.region_suffix}"
}

# NOTE — the gitea_backup bucket + key + outputs were removed in the
# apl-core anti-pattern cleanup. The off-cluster gitea backup integration
# was blocked by apl-core's list-REPLACE behaviour on _rawValues.
# extraVolumes (it clobbered the chart-injected custom-ca volume); rather
# than carry the unused infrastructure pending the kustomize post-renderer
# follow-up, the bucket + key + outputs + bao policy paths + GH-secret
# seeding were all dropped. See apl-values/primary/values.yaml apps.gitea
# for the full rationale.

# ── 120-day rotation clock ─────────────────────────────────────────────────────
# Linode Object Storage keys have no native expiry — the only way to "revoke
# after 120 days" (LKE Secrets Rotation Guidelines) is to destroy and recreate
# the key. rotation_rfc3339 advances by var.obj_key_rotation_days once the
# window elapses; the next `terraform apply` of this module then forces the
# access key below to be replaced (old key destroyed = revoked). Routine
# applies inside the window are a no-op.

resource "time_rotating" "loki_key" {
  rotation_days = var.obj_key_rotation_days
}

# ── Scoped access key for Loki ─────────────────────────────────────────────────
# Read/write key limited to the three Loki buckets. The key credentials are
# sensitive outputs; store them as LOKI_S3_ACCESS_KEY / LOKI_S3_SECRET_KEY
# in the infra-<region> GitHub environment, then run bootstrap-openbao.yml to
# seed secret/loki/object-store in OpenBao.
#
# Rotation is NOT zero-touch: replacement mints new credentials but the
# GitHub-env-secret → bootstrap-openbao reseed hop is manual (see
# docs/runbooks/linode-credential-rotation.md). The loki-objkey-rotation-health
# check alerts if the OpenBao secret falls behind this 120-day clock.

resource "linode_object_storage_key" "loki" {
  label = "${var.label_prefix}-loki-${var.region_suffix}"

  bucket_access {
    bucket_name = linode_object_storage_bucket.loki_chunks.label
    region      = local.obj_region
    permissions = "read_write"
  }

  bucket_access {
    bucket_name = linode_object_storage_bucket.loki_ruler.label
    region      = local.obj_region
    permissions = "read_write"
  }

  bucket_access {
    bucket_name = linode_object_storage_bucket.loki_admin.label
    region      = local.obj_region
    permissions = "read_write"
  }

  lifecycle {
    replace_triggered_by = [time_rotating.loki_key.rotation_rfc3339]
  }
}

# ── Scoped access key for Harbor registry ─────────────────────────────────────
# Read/write key limited to the harbor_registry bucket. Same 120-day rotation
# clock as the Loki + Gitea-backup keys; deliberately a separate scoped key
# (not extending the Loki key) so a Harbor-side leak doesn't expose telemetry
# storage and vice versa. Credentials are manually seeded via
# bootstrap-openbao.yml's "Seed Harbor registry S3 credentials in OpenBao"
# step.
resource "linode_object_storage_key" "harbor_registry" {
  label = "${var.label_prefix}-harbor-registry-${var.region_suffix}"

  bucket_access {
    bucket_name = linode_object_storage_bucket.harbor_registry.label
    region      = local.obj_region
    permissions = "read_write"
  }

  lifecycle {
    replace_triggered_by = [time_rotating.loki_key.rotation_rfc3339]
  }
}
