output "loki_access_key" {
  description = "Loki Object Storage access key. Store as LOKI_S3_ACCESS_KEY in the infra-<region> GitHub environment, then run bootstrap-openbao.yml."
  value       = linode_object_storage_key.loki.access_key
  sensitive   = true
}

output "loki_secret_key" {
  description = "Loki Object Storage secret key. Store as LOKI_S3_SECRET_KEY in the infra-<region> GitHub environment, then run bootstrap-openbao.yml."
  value       = linode_object_storage_key.loki.secret_key
  sensitive   = true
}

output "bucket_names" {
  description = "Loki bucket labels (chunks / ruler / admin) for this region."
  value = {
    chunks = linode_object_storage_bucket.loki_chunks.label
    ruler  = linode_object_storage_bucket.loki_ruler.label
    admin  = linode_object_storage_bucket.loki_admin.label
  }
}

output "harbor_registry_bucket" {
  description = "Harbor registry bucket label for this region. Consumed by Harbor's chart via apps.harbor._rawValues.persistence.imageChartStorage.s3.bucket and by the harbor-registry-s3 ExternalSecret as bucket_name."
  value       = linode_object_storage_bucket.harbor_registry.label
}

output "harbor_registry_access_key" {
  description = "Harbor registry Object Storage access key. Store as HARBOR_REGISTRY_S3_ACCESS_KEY in the infra-<region> GitHub environment, then run bootstrap-openbao.yml."
  value       = linode_object_storage_key.harbor_registry.access_key
  sensitive   = true
}

output "harbor_registry_secret_key" {
  description = "Harbor registry Object Storage secret key. Store as HARBOR_REGISTRY_S3_SECRET_KEY in the infra-<region> GitHub environment, then run bootstrap-openbao.yml."
  value       = linode_object_storage_key.harbor_registry.secret_key
  sensitive   = true
}

output "s3_endpoint" {
  description = "S3-compatible endpoint URL for this region's Object Storage. Derived from var.obj_cluster (which is the Linode-OBJ region code like 'us-ord-1') so destroy-time bucket drain in .github/workflows/terraform.yml can target the correct endpoint without re-reading the tfvars."
  value       = "https://${var.obj_cluster}.linodeobjects.com"
}

output "loki_key_rotates_at" {
  description = "RFC3339 timestamp when the Loki Object Storage key is next force-rotated (120-day SLA). Surfaced by the loki-objkey-rotation-health check."
  value       = time_rotating.loki_key.rotation_rfc3339
}

output "next_steps" {
  description = "Post-apply (and post-rotation) checklist."
  value       = <<-EOT

    ── Post-apply / post-rotation checklist ────────────────────────────────

    Run these on first apply AND every time the 120-day clock recreates the
    key (a plan showing linode_object_storage_key.loki must-replace). The
    reseed hop is manual — skipping it leaves Loki on a revoked key.

    1. Extract the Loki credentials from Terraform state:

         terraform output -raw loki_access_key
         terraform output -raw loki_secret_key

    2. Extract the Harbor registry credentials from Terraform state:

         terraform output -raw harbor_registry_access_key
         terraform output -raw harbor_registry_secret_key

    3. Store all four values as GitHub environment secrets in infra-<region>:
         LOKI_S3_ACCESS_KEY
         LOKI_S3_SECRET_KEY
         HARBOR_REGISTRY_S3_ACCESS_KEY
         HARBOR_REGISTRY_S3_SECRET_KEY

    4. Run bootstrap-openbao.yml for the region to reseed
       secret/loki/object-store + secret/harbor/registry-s3 so the
       loki-object-store and harbor-registry-s3 ExternalSecrets sync the
       new K8s Secrets, then restart Loki / harbor-registry to pick up
       the rotated keys.

    Next forced rotation: ${time_rotating.loki_key.rotation_rfc3339}
    Full procedure: docs/runbooks/linode-credential-rotation.md

    ────────────────────────────────────────────────────────────────────────
  EOT
}
