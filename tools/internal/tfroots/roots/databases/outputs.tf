# Re-export the module outputs so `llz ci seed-db-admin` (and operators) can read
# the admin connections from `terraform output` after apply.
#
# Every output is a MAP KEYED BY CLUSTER NAME, because this root provisions 0-n
# clusters. An empty `databases` variable yields empty maps, not an error — which
# is what lets the seed command run unconditionally on a deployment that declared
# no databases and simply find nothing to seed.

output "database_ids" {
  description = "Linode Managed Database ID per cluster name."
  value       = { for name, db in module.databases : name => db.database_id }
}

output "labels" {
  description = "Provisioned cluster label per cluster name (\"<label_prefix>-<name>-<region_suffix>\")."
  value       = { for name, db in module.databases : name => db.label }
}

output "hosts" {
  description = "Primary (VPC-internal) connection host per cluster name."
  value       = { for name, db in module.databases : name => db.host }
}

output "ports" {
  description = "Connection port per cluster name."
  value       = { for name, db in module.databases : name => db.port }
}

# The one `llz ci seed-db-admin` reads: a single `terraform output -json
# connections` carries everything secret/platform/db-admin/<name> needs, so the
# seed is one read per apply rather than one per field per cluster — and it
# cannot pair a host with the wrong cluster's password.
#
# Marked sensitive because it CONTAINS sensitive members (root_username is
# provider-marked sensitive, plus the password and CA); Terraform would reject it
# unmarked. `terraform output -json connections` still reads it in full.
output "connections" {
  description = "Full admin connection per cluster name — { endpoint, port, username, password, ca }. Seeded to secret/platform/db-admin/<name>. `ca` is base64-encoded: decode before writing it to a trust file."
  sensitive   = true
  value = {
    for name, db in module.databases : name => {
      endpoint = db.host
      port     = db.port
      username = db.root_username
      password = db.root_password
      ca       = db.ca_cert
    }
  }
}
