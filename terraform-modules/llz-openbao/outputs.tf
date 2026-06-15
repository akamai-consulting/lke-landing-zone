output "approle_role_id" {
  description = "The role_id for the CI AppRole. Used in ESO ClusterSecretStore and CI secrets."
  value       = vault_approle_auth_backend_role.ci.role_id
}

output "ci_role_name" {
  description = "The CI AppRole / policy name (echoes the input; convenient for the rotation workflow + next_steps)."
  value       = var.ci_role_name
}

output "next_steps" {
  description = "Post-apply checklist."
  value       = <<-EOT

    ── Post-apply checklist ────────────────────────────────────────────────

    1. Generate an AppRole secret_id and seed ESO + GitHub CI secrets
       (replace the pod name with your OpenBao release's pod-0):

         kubectl -n llz-openbao exec -it <openbao-pod-0> -- \
           env VAULT_ADDR=https://127.0.0.1:8200 VAULT_SKIP_VERIFY=true \
           bao write -f auth/approle/role/${var.ci_role_name}/secret-id -format=json

       Then create the eso-approle-secret:

         kubectl -n llz-external-secrets create secret generic eso-approle-secret \
           --from-literal=secretId=<secret_id>

    2. Set GitHub Actions secrets:
         OPENBAO_APPROLE_ROLE_ID   = ${vault_approle_auth_backend_role.ci.role_id}
         OPENBAO_APPROLE_SECRET_ID = <secret_id from above>

       (The bootstrap-openbao.yml workflow handles both steps automatically.)

    3. Revoke the root/admin token used for this apply:

         bao token revoke -self

    ────────────────────────────────────────────────────────────────────────
  EOT
}
