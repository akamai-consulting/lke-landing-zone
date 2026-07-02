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
# the LKE account-quota orphan problem (see docs/lessons-learned.md).
# NOTE: an earlier commit added `force_destroy = true` here to auto-empty
# buckets before `terraform destroy` — the Linode provider's
# linode_object_storage_bucket resource does NOT support that argument
# (validated against the deployed provider version).
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

# ── Access keys — NOT Terraform-managed ───────────────────────────────────────
# The scoped Loki/Harbor access keys (and their 120-day `time_rotating`
# replacement clock) were REMOVED from this module. Key lifecycle now has ONE
# owner end to end, outside Terraform:
#
#   - first boot:  `llz ci mint-bootstrap-objkeys` (llz-bootstrap-openbao.yml)
#     mints the scoped keys via the Linode API and seeds
#     secret/loki/object-store + secret/harbor/registry-s3 in OpenBao directly
#     (rotated_at-stamped). No LOKI_S3_* / HARBOR_REGISTRY_S3_* GitHub secrets,
#     no stash/reseed relay.
#   - rotation:    the in-cluster linodeCredRotator CronJob
#     (`llz ci rotate-linode-creds`) mints replacements when due and drains
#     older same-labeled keys.
#
# WHY Terraform could not keep the keys: the rotator drains SAME-LABELED keys,
# so a TF-tracked key is drained on the rotator's second rotation and TF
# recreates it on the next apply — a permanent tug-of-war (see
# docs/designs/linode-credential-rotator.md). Buckets stay here (stable,
# never rotated); keys don't.
#
# Destroy-time bucket drain: terraform.yml's destroy-object-storage job mints a
# TEMPORARY scoped key (`llz ci temp-objkey create`) for the s5cmd drain and
# deletes it afterwards — it no longer reads key credentials from TF outputs.
