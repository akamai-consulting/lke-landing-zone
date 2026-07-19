# Re-exports the module outputs unchanged so the CI workflows (terraform.yml
# destroy-time bucket drain, bootstrap summaries) and the documented
# `terraform output …` commands keep working verbatim.
#
# (The loki_*/harbor_registry_* KEY outputs and loki_key_rotates_at were
# REMOVED with the TF-managed access keys: `llz ci mint-bootstrap-objkeys`
# mints + seeds the credentials at bootstrap and the in-cluster
# linodeCredRotator owns rotation — no key material lives in this state.)

output "bucket_names" {
  description = "Loki bucket labels (chunks / ruler / admin) for this region."
  value       = module.object_storage.bucket_names
}

output "harbor_registry_bucket" {
  description = "Harbor registry bucket label for this region. Consumed by Harbor's chart via apps.harbor._rawValues.persistence.imageChartStorage.s3.bucket and by the harbor-registry-s3 ExternalSecret as bucket_name."
  value       = module.object_storage.harbor_registry_bucket
}

output "s3_endpoint" {
  description = "S3-compatible endpoint URL for this region's Object Storage. Derived from var.obj_cluster so destroy-time bucket drain in .github/workflows/terraform.yml can target the correct endpoint without re-reading the tfvars."
  value       = module.object_storage.s3_endpoint
}
