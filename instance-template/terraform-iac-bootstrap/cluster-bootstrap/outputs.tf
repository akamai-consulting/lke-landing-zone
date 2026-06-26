output "next_steps" {
  description = "Post-apply checklist printed at the end of every apply."
  value       = <<-EOT

    ── Post-apply checklist ────────────────────────────────────────────────

    Apl-core is installed. The apl-operator drives the helmfile pipeline
    which installs ~40 components over 10–15 minutes; downstream readiness
    is observed via Argo CD, not Terraform.

    1. Watch the apl-operator log:

         kubectl -n apl-operator logs -l app.kubernetes.io/name=apl-operator -f

    2. Retrieve the Console URL and platform-admin credentials:

         kubectl get configmap welcome -n apl-operator -o jsonpath='{.data.consoleUrl}'
         kubectl get secret platform-admin-credentials -n keycloak \
           -o jsonpath='{.data.username}' | base64 -d ; echo
         kubectl get secret platform-admin-credentials -n keycloak \
           -o jsonpath='{.data.password}' | base64 -d ; echo

    3. Confirm Argo CD has reconciled the in-repo manifest/ tree
       (OpenBao, ESO ClusterSecretStore, cert-manager extras,
       firewall-controller suspended, Argo Workflows + Events
       cert-automation, Istio Gateway routes for OTel and Harbor):

         kubectl -n argocd get applications

    4. Once OpenBao pods are Running, bootstrap OpenBao:

         .github/workflows/bootstrap-openbao.yml → workflow_dispatch
         region: ${var.deployment}

    ────────────────────────────────────────────────────────────────────────
  EOT
}
