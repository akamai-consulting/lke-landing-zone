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

# The Linode API REJECTS the deprecated `cluster` argument on bucket creation
# ([400] cluster is not valid) — a bucket is placed by `region` (the cluster id
# with the trailing `-N` ordinal stripped: `us-ord-10` → `us-ord`).
#
# BUT a region can expose SEVERAL endpoints of different generations, and a
# bucket is PINNED to the one it is created against — reachable there and
# NOWHERE else (a request for the same bucket at a sibling endpoint 404s
# NoSuchBucket). The endpoint types (techdocs.akamai.com/cloud-computing/docs/
# endpoint-types): E0/E1 legacy, E2/E3 standard/gen-2. Crucially the hostname
# ordinal does NOT encode the type — Chicago has BOTH `us-ord-1` (E1) and
# `us-ord-10` (E3). So placing by `region` ALONE lands the bucket on the
# region's DEFAULT (gen-1 / E1 = `us-ord-1`), while every consumer derives its
# S3 host from the full var.obj_cluster (`us-ord-10.linodeobjects.com`) — a
# split that silently breaks Loki/Harbor with NoSuchBucket even though the API
# lists the bucket as present.
#
# Fix: pin each bucket to the requested endpoint by passing `s3_endpoint`
# (a first-class create input — linodego ObjectStorageBucketCreateOptions.
# S3Endpoint, surfaced by the provider as an Optional/ForceNew attribute), so
# the full obj_cluster host is honoured end to end. No endpoint_type lookup
# needed: s3_endpoint names the exact host and a wrong one fails loudly at apply.
locals {
  obj_region  = replace(var.obj_cluster, "/-[0-9]+$/", "")
  s3_endpoint = "${var.obj_cluster}.linodeobjects.com"
}

resource "linode_object_storage_bucket" "harbor_registry" {
  region      = local.obj_region
  s3_endpoint = local.s3_endpoint
  label       = "${var.label_prefix}-harbor-registry-${var.region_suffix}"
}

# ── Loki object-storage buckets ───────────────────────────────────────────────
# Three buckets per region matching the names in loki-values.yaml (and the
# secondary override in loki-values-secondary.yaml). All buckets are private.

resource "linode_object_storage_bucket" "loki_chunks" {
  region      = local.obj_region
  s3_endpoint = local.s3_endpoint
  label       = "${var.label_prefix}-loki-chunks-${var.region_suffix}"
}

resource "linode_object_storage_bucket" "loki_ruler" {
  region      = local.obj_region
  s3_endpoint = local.s3_endpoint
  label       = "${var.label_prefix}-loki-ruler-${var.region_suffix}"
}

resource "linode_object_storage_bucket" "loki_admin" {
  region      = local.obj_region
  s3_endpoint = local.s3_endpoint
  label       = "${var.label_prefix}-loki-admin-${var.region_suffix}"
}

# NOTE — the gitea_backup bucket + key + outputs were removed in the
# apl-core anti-pattern cleanup. The off-cluster gitea backup integration
# was blocked by apl-core's list-REPLACE behaviour on _rawValues.
# extraVolumes (it clobbered the chart-injected custom-ca volume); rather
# than carry the unused infrastructure pending the kustomize post-renderer
# follow-up, the bucket + key + outputs + bao policy paths + GH-secret
# seeding were all dropped. (This is separate from the gitea-valkey PVC
# encryption the Kyverno pvc-force-encrypted-storage-class policy still covers
# for an opt-in gitea — that stays; only the off-cluster S3 backup was dropped.)

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
