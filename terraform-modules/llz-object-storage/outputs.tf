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

output "s3_endpoint" {
  description = "S3-compatible endpoint URL for this region's Object Storage. Derived from var.obj_cluster (which is the Linode-OBJ region code like 'us-ord-1') so destroy-time bucket drain in .github/workflows/terraform.yml can target the correct endpoint without re-reading the tfvars."
  value       = "https://${var.obj_cluster}.linodeobjects.com"
}

# (loki_access_key / loki_secret_key / harbor_registry_access_key /
# harbor_registry_secret_key / loki_key_rotates_at were REMOVED with the
# TF-managed keys — see main.tf's "Access keys" note. Credentials are minted by
# `llz ci mint-bootstrap-objkeys` + rotated in-cluster, and never transit
# Terraform state or GitHub secrets.)
