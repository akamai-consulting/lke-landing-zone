output "database_id" {
  description = "Linode Managed Database ID."
  value       = linode_database_postgresql_v2.shared.id
}

output "label" {
  description = "The database cluster label."
  value       = linode_database_postgresql_v2.shared.label
}

output "host" {
  description = "Primary connection host. With private_network attachment (public_access=false) this is the VPC-internal endpoint reached from workloads in the same VPC. Seeded to secret/platform/db-admin as `endpoint`."
  value       = linode_database_postgresql_v2.shared.host_primary
}

output "host_standby" {
  description = "Standby connection host (populated for HA cluster_size >= 2)."
  value       = linode_database_postgresql_v2.shared.host_standby
}

output "port" {
  description = "Connection port. Seeded to secret/platform/db-admin as `port`."
  value       = linode_database_postgresql_v2.shared.port
}

output "root_username" {
  description = "Admin username (`akmadmin` on the Aiven-backed platform). Seeded to secret/platform/db-admin as `username`."
  value       = linode_database_postgresql_v2.shared.root_username
  # The provider marks root_username sensitive, so re-exporting it unmarked is a
  # hard `tofu validate` error ("Output refers to sensitive values") whenever this
  # module is validated as a root — which `make tf-validate` does. Not a secret in
  # any real sense (it is a fixed platform username), but the annotation is
  # required, and `terraform output -json` still reads it for `llz ci seed-db-admin`.
  sensitive = true
}

output "root_password" {
  description = "Admin password. Seeded to secret/platform/db-admin as `password`. Sensitive — never surfaced in logs/plan output."
  value       = linode_database_postgresql_v2.shared.root_password
  sensitive   = true
}

output "ca_cert" {
  description = "Base64-encoded server CA certificate. Seeded to secret/platform/db-admin as `ca` (decode before use). Sensitive."
  value       = linode_database_postgresql_v2.shared.ca_cert
  sensitive   = true
}

output "engine_version" {
  description = "The provisioned PostgreSQL engine version."
  value       = linode_database_postgresql_v2.shared.version
}
