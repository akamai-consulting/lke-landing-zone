# Inputs the thin cluster-bootstrap root passes into this module. The root owns
# the instance-relative reads (the apl-values templatefile, the env-revision
# configmap) and the configured helm/kubernetes/kubectl providers; this module
# owns the provisioning resources those inputs feed. See the root's main.tf +
# providers.tf for where each value originates.

variable "apl_rendered_values" {
  description = "The fully rendered apl-core values.yaml CONTENT (the root's templatefile result — runtime secrets already injected into otomi.git.password, dns.provider.linode.apiToken, the loki admin password, and the live coredns ClusterIP). local_file.apl_rendered_values dumps it to disk for diagnostics and helm_release.apl installs from it. Sensitive because it carries the values-repo PAT + DNS token."
  type        = string
  sensitive   = true
}

variable "env_revision_in_configmap" {
  description = "data.revision parsed by the root from apl-values/<env>/manifest/env-revision-configmap.yaml. local_file.apl_rendered_values' precondition asserts this equals apps_repo_revision so the bootstrap App and its in-repo child Apps target the same branch."
  type        = string
}

variable "kubeconfig_raw" {
  description = "Raw kubeconfig YAML for the target cluster (from the root's local.kubeconfig_raw, normalized to \"\" when the cluster workspace state is empty/null). Passed as KUBECONFIG_RAW to the llz-ci provisioners (wait-apl-pipeline, apply-kyverno-policy). Sensitive: embeds the cluster bearer token."
  type        = string
  sensitive   = true
}

# ── Akamai App Platform (apl-core) ────────────────────────────────────────────

variable "apl_chart_version" {
  description = "Pinned apl Helm chart version. Bump intentionally in tfvars to upgrade — do not leave empty. Check latest: helm search repo apl/apl --versions"
  type        = string
}

variable "apl_values_env" {
  description = "Subdirectory under apl-values/ for this cluster (lab, staging, primary, secondary). Names the generated rendered-values dump and the Argo CD bootstrap App's source path (apl-values/<env>/manifest)."
  type        = string
}

variable "apps_repo_revision" {
  description = "Branch, tag, or commit of the instance repo that the TF-managed bootstrap ArgoCD Applications (platform-bootstrap + llz-secret-store) target. Also the value the env-revision precondition asserts the configmap matches. Defaults to 'main' at the root."
  type        = string
}

variable "instance_repo" {
  description = "This instance's GitHub owner/name (e.g. akamai-consulting/lke-landing-zone-example). The TF-managed bootstrap ArgoCD Application + AppProject source their manifest tree from https://github.com/<instance_repo>.git. Passed by the (copier/tfroots-rendered) root — this module is git-fetched and is NOT rendered, so it cannot carry the <@ instance_repo @> token itself."
  type        = string
}

variable "upstream_org" {
  description = "GitHub org that publishes the first-party OCI Helm charts (ghcr.io/<upstream_org>/charts/*). Passed by the rendered root for the same reason as instance_repo: the git-fetched module cannot carry the <@ upstream_org @> token. Only consumed on the private-fork path (ghcr_token set)."
  type        = string
  default     = "akamai-consulting"
}

variable "ghcr_username" {
  description = "GitHub username that owns the GHCR read token (ghcr_token). GHCR OCI auth needs a real account name (the PAT owner), not a placeholder. Empty default = the ArgoCD GHCR repo Secret + image-pull Secret are skipped (kept optional so plan/destroy work without it)."
  type        = string
  default     = ""
}

variable "ghcr_token" {
  description = "OPTIONAL GitHub token for GHCR. The first-party OCI Helm charts are PUBLIC, so ArgoCD pulls them anonymously and this is normally left empty. Set it only for a private fork that keeps its charts private, or the optional Akamai-internal firewall-controller-internal image. When set, pair with ghcr_username. Empty default = no GHCR Secret created (the public-charts path)."
  type        = string
  sensitive   = true
  default     = ""
}
