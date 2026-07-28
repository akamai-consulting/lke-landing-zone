# Re-export the module outputs so `llz ci seed-db-admin` (and operators) can read
# the admin connection from `terraform output` after apply. root_password + ca_cert
# are sensitive and never printed in plan/apply logs.
output "database_id" {
  description = "Linode Managed Database ID."
  value       = module.databases.database_id
}

output "host" {
  description = "Primary (VPC-internal) connection host. Seeded to secret/platform/db-admin as `endpoint`."
  value       = module.databases.host
}

output "port" {
  description = "Connection port. Seeded as `port`."
  value       = module.databases.port
}

output "root_username" {
  description = "Admin username. Seeded as `username`."
  value       = module.databases.root_username
}

output "root_password" {
  description = "Admin password. Seeded as `password`."
  value       = module.databases.root_password
  sensitive   = true
}

output "ca_cert" {
  description = "Base64-encoded server CA. Seeded as `ca`."
  value       = module.databases.ca_cert
  sensitive   = true
}
