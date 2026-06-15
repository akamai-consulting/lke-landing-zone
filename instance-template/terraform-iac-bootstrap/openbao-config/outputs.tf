# Re-exports the module outputs unchanged so bootstrap-openbao.yml and the
# documented `terraform output` commands keep working verbatim.

output "approle_role_id" {
  description = "The role_id for the CI AppRole. Used in ESO ClusterSecretStore and CI secrets."
  value       = module.openbao_config.approle_role_id
}

output "next_steps" {
  description = "Post-apply checklist."
  value       = module.openbao_config.next_steps
}
