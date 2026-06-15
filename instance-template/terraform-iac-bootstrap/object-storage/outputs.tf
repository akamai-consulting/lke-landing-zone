# Re-exports the module outputs unchanged so the CI workflows (terraform.yml
# destroy-time bucket drain, bootstrap-openbao.yml credential seeding) and the
# documented `terraform output -raw …` commands keep working verbatim.

output "loki_access_key" {
  description = "Loki Object Storage access key. Store as LOKI_S3_ACCESS_KEY in the infra-<region> GitHub environment, then run bootstrap-openbao.yml."
  value       = module.object_storage.loki_access_key
  sensitive   = true
}

output "loki_secret_key" {
  description = "Loki Object Storage secret key. Store as LOKI_S3_SECRET_KEY in the infra-<region> GitHub environment, then run bootstrap-openbao.yml."
  value       = module.object_storage.loki_secret_key
  sensitive   = true
}

output "bucket_names" {
  description = "Loki bucket labels (chunks / ruler / admin) for this region."
  value       = module.object_storage.bucket_names
}

output "harbor_registry_bucket" {
  description = "Harbor registry bucket label for this region. Consumed by Harbor's chart via apps.harbor._rawValues.persistence.imageChartStorage.s3.bucket and by the harbor-registry-s3 ExternalSecret as bucket_name."
  value       = module.object_storage.harbor_registry_bucket
}

output "harbor_registry_access_key" {
  description = "Harbor registry Object Storage access key. Store as HARBOR_REGISTRY_S3_ACCESS_KEY in the infra-<region> GitHub environment, then run bootstrap-openbao.yml."
  value       = module.object_storage.harbor_registry_access_key
  sensitive   = true
}

output "harbor_registry_secret_key" {
  description = "Harbor registry Object Storage secret key. Store as HARBOR_REGISTRY_S3_SECRET_KEY in the infra-<region> GitHub environment, then run bootstrap-openbao.yml."
  value       = module.object_storage.harbor_registry_secret_key
  sensitive   = true
}

output "s3_endpoint" {
  description = "S3-compatible endpoint URL for this region's Object Storage. Derived from var.obj_cluster so destroy-time bucket drain in .github/workflows/terraform.yml can target the correct endpoint without re-reading the tfvars."
  value       = module.object_storage.s3_endpoint
}

output "loki_key_rotates_at" {
  description = "RFC3339 timestamp when the Loki Object Storage key is next force-rotated (120-day SLA). Surfaced by the loki-objkey-rotation-health check."
  value       = module.object_storage.loki_key_rotates_at
}

output "next_steps" {
  description = "Post-apply (and post-rotation) checklist."
  value       = module.object_storage.next_steps
}
