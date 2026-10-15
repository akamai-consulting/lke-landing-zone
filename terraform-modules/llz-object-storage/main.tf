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

# ── OpenBao Raft snapshot bucket ──────────────────────────────────────────────
# Holds the scheduled `bao operator raft snapshot` archives the openbaoBackup
# CronJob writes (`llz ci openbao-backup`), and is what `llz openbao restore`
# reads back. A SEPARATE bucket, not a prefix inside a Loki/Harbor one, because
# Linode Object Storage scopes keys per BUCKET: only a dedicated bucket lets the
# backup key (`<prefix>-openbao-backup-<suffix>`, minted by llz, never by
# Terraform) reach the snapshots and nothing else, and keeps the broad platform
# key — which apl-core hands to Loki and Harbor — from being able to delete them.
#
# Confidentiality does NOT rest on this bucket being private (it is, like every
# bucket here): a snapshot is ciphertext under OpenBao's barrier keyring, which
# only the deployment's static seal key (OPENBAO_SEAL_KEY) can unwrap. Linode OBJ
# offers no managed keys, and none are needed for that. What the bucket DOES leak
# to someone holding it is storage METADATA — see docs/runbooks/openbao-backup-restore.md.
#
# Label infix `openbao-backup` (16 chars with its hyphens) is deliberately no
# longer than `-harbor-registry-` (17), the infix clusterspec's label-length
# validation budgets for — a longer one would silently reject instance/deployment
# name pairs that validate today.
#
# VERSIONED, WITH NONCURRENT VERSIONS EXPIRED. Retention (the CronJob's prune)
# deletes snapshots on purpose, and the restore path reads only current objects —
# so a retention bug, a mistaken hand delete, or an overwrite would otherwise be
# final. With versioning a deleted snapshot is a noncurrent version for
# var.openbao_backup_noncurrent_days and can be copied back. It does NOT protect
# against a holder of the backup key or LINODE_API_TOKEN deliberately deleting
# versions; the runbook says so.
#
# Both arguments need S3 credentials at plan/apply time (they are S3 calls, not
# Linode API calls). The calling root sets `obj_use_temp_keys = true` on its
# provider, which mints a short-lived key per operation from the Linode token the
# apply already holds; a root that does not will fail the apply asking for
# access_key/secret_key, which is the loud failure we want.
#
# Its contents also survive the deployment: destroying object-storage drains this
# bucket only when the caller opts in (drain_data_buckets), and otherwise refuses
# before anything is deleted. See llz-terraform.yml's destroy-object-storage job.
resource "linode_object_storage_bucket" "openbao_backup" {
  region      = local.obj_region
  s3_endpoint = local.s3_endpoint
  label       = "${var.label_prefix}-openbao-backup-${var.region_suffix}"

  versioning = true

  lifecycle_rule {
    id      = "expire-noncurrent-snapshots"
    enabled = true
    # No prefix: everything in this bucket is a snapshot (or an operator's note,
    # which is no worse off kept for the window too).
    noncurrent_version_expiration {
      days = var.openbao_backup_noncurrent_days
    }
    # The producer uploads with a single PUT, so there are no multipart uploads to
    # abandon — but a hand-run `aws s3 cp` of a large snapshot would be one.
    abort_incomplete_multipart_upload_days = 1
  }
}

# ── Velero backup bucket ──────────────────────────────────────────────────────
# Holds what the `velero` component's Velero server writes: one tarball of
# Kubernetes resources per backup (every included namespace's objects, its
# Secrets among them), plus Velero's metadata, logs and result files beside it.
# Resources only — no volume data (the component does not snapshot PVs; see
# docs/runbooks/velero-dr.md, "What is not backed up").
#
# ITS OWN BUCKET, for the same reason as openbao_backup: Linode scopes keys per
# bucket, so only a dedicated bucket lets Velero's key (`<prefix>-velero-<suffix>`,
# minted by llz, never by Terraform) reach these backups and nothing else, and
# keeps the broad platform key apl-core hands to Loki and Harbor from reading —
# or deleting — a copy of every included namespace's Secrets.
#
# CREATED ON EVERY DEPLOYMENT, even where spec.components.velero is off. An empty
# bucket costs nothing and holds nothing; the key is what grants access, and llz
# mints it only where the component is enabled. One unconditional bucket set
# keeps clusterspec.ObjBucketLabels — what the bucket-presence preflights read —
# a single list rather than a per-component one that can drift from this file.
#
# CONFIDENTIALITY IS SSE-C, applied by Velero itself. Velero's AWS object-store
# plugin sends the SSE-C headers on every PUT/GET (customerKeyEncryptionSecret),
# with a 32-byte key llz generates once into OpenBao (secret/obj/velero-ssec) and
# ESO projects into the llz-velero namespace. Linode encrypts each object under
# it and keeps no copy, so a holder of this bucket's key alone reads nothing.
# Not the objProxy gateway: Velero can emit SSE-C natively, so the backup path
# does not take a dependency on a default-off component sitting on Loki's and
# Harbor's write path. The key lives in OpenBao, which is restored from its OWN
# Raft snapshots (openbao_backup above, never from Velero), so a lost cluster
# recovers OpenBao first and then has the key to read these backups.
#
# VERSIONED, WITH NONCURRENT VERSIONS EXPIRED, like openbao_backup: Velero's
# garbage collector deletes a backup's objects when its TTL lapses, and a
# GC bug, a mistaken `velero backup delete`, or an overwrite would otherwise be
# final. A deleted backup stays recoverable as noncurrent versions for
# var.velero_noncurrent_days (copying one back needs the SSE-C key, as reading
# any of them does). It does NOT protect against a holder of the velero key or
# LINODE_API_TOKEN deleting versions on purpose; the runbook says so. Both
# arguments are S3 calls — the calling root's provider needs obj_use_temp_keys.
#
# Its contents also survive the deployment: a module=object-storage destroy
# refuses while this bucket holds any version unless the caller opts in with
# drain_data_buckets (`llz ci openbao-backup-destroy-guard` guards both backup
# buckets).
resource "linode_object_storage_bucket" "velero" {
  region      = local.obj_region
  s3_endpoint = local.s3_endpoint
  label       = "${var.label_prefix}-velero-${var.region_suffix}"

  versioning = true

  lifecycle_rule {
    id      = "expire-noncurrent-backups"
    enabled = true
    # No prefix: everything in this bucket is Velero's (backups/, restores/,
    # and the metadata beside them).
    noncurrent_version_expiration {
      days = var.velero_noncurrent_days
    }
    # Velero's plugin uploads large tarballs MULTIPART (the AWS SDK upload
    # manager); a Velero pod killed mid-upload leaves parts that are billed and
    # invisible to every LIST. Abort them after a day.
    abort_incomplete_multipart_upload_days = 1
  }
}

# ── CloudNativePG backup bucket ───────────────────────────────────────────────
# Holds apl-core's CNPG database backups when the `cnpgBackup` component is on:
# barman-cloud base backups and the continuous WAL archive of the Harbor
# (harbor-otomi-db) and Keycloak (keycloak-db) Postgres clusters, under
# s3://<this bucket>/<db>-<generation>/<cluster>/{base,wals}/. apl-core writes it
# — LLZ only points apl-core's NATIVE platformBackups settings at it (the
# AplObjectStorage CR's buckets.cnpg + env/settings/platformBackups.yaml); see
# docs/runbooks/cnpg-backup-restore.md.
#
# NOT ITS OWN KEY, and that is the one thing about this bucket that differs from
# openbao_backup and velero above. apl-core's otomi-db chart reads ONE credential
# for every app it runs — the `linode-creds` Secret it builds from
# obj.provider.linode.accessKeyId plus obj-secrets — so the barman sidecar can
# only authenticate with the platform key llz mints for Loki and Harbor
# (`<prefix>-obj-<suffix>`, rotation-table entry obj-platform), and that key is
# scoped to this bucket too. The consequence, stated plainly: anything holding
# the platform key — Loki, Harbor, anyone who can read obj-secrets — can read
# and DELETE these database backups. The dedicated-key answer the OpenBao and
# Velero buckets use is not available, because apl-core exposes no per-app
# object-storage credential to point at one.
#
# VERSIONED, WITH NONCURRENT VERSIONS EXPIRED, AND THAT IS THE MITIGATION. A
# delete or overwrite by the shared key — a Loki or Harbor bug, a compromised
# pod, barman's own retention — leaves the object recoverable as a noncurrent
# version for var.cnpg_noncurrent_days. 30 days, longer than the velero bucket's
# 14, BECAUSE the key is shared: the window is how long a destructive write by
# something that was never meant to touch these objects stays reversible. It
# does NOT protect against a key holder deleting versions on purpose (Linode has
# no object lock); the runbook says so. Both arguments are S3 calls — the calling
# root's provider needs obj_use_temp_keys.
#
# ENCRYPTION AT REST FOLLOWS objProxy. barman-cloud can emit SSE-C, but
# apl-core's chart exposes no setting for it. barman addresses the endpoint
# path-style, so with spec.components.objProxy on its requests are captured by the
# proxy's CoreDNS rewrite and written SSE-C (the objProxy component trusts its CA
# into apl-core's ObjectStores for exactly this); without objProxy the objects are
# plaintext at rest. Registered with the at-rest guard for that reason.
#
# CREATED ON EVERY DEPLOYMENT, like velero: an empty bucket costs nothing, and
# one unconditional bucket set keeps clusterspec.ObjBucketLabels a single list.
#
# Its contents survive the deployment: a module=object-storage destroy refuses
# while this bucket holds any version unless the caller opts in with
# drain_data_buckets (`llz ci openbao-backup-destroy-guard` guards it).
resource "linode_object_storage_bucket" "cnpg" {
  region      = local.obj_region
  s3_endpoint = local.s3_endpoint
  label       = "${var.label_prefix}-cnpg-${var.region_suffix}"

  versioning = true

  lifecycle_rule {
    id      = "expire-noncurrent-backups"
    enabled = true
    # No prefix: everything in this bucket is barman's (one <db>-<generation>/
    # prefix per database).
    noncurrent_version_expiration {
      days = var.cnpg_noncurrent_days
    }
    # barman-cloud uploads base backups MULTIPART (boto3's transfer manager); an
    # instance killed mid-backup leaves parts that are billed and invisible to
    # every LIST. Abort them after a day.
    abort_incomplete_multipart_upload_days = 1
  }
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
