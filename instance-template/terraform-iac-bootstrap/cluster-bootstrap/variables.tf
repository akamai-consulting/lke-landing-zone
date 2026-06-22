variable "deployment" {
  description = "Deployment discriminator (e.g. primary, secondary, lab) used to locate the matching cluster workspace state — primary/secondary are prod's DR pair while lab is a separate environment tier. Must match the cluster workspace's backend key suffix. (Renamed from `region`, which collided with the cluster root's literal Linode region.)"
  type        = string
}

variable "tf_state_bucket" {
  description = "S3 bucket holding the cluster workspace's Terraform state. Supply via TF_VAR_tf_state_bucket from CI."
  type        = string
}

# ── Akamai App Platform (apl-core) ────────────────────────────────────────────

variable "apl_chart_version" {
  description = "Pinned apl Helm chart version. Bump intentionally in tfvars to upgrade — do not leave empty. Check latest: helm search repo apl/apl --versions"
  type        = string
}

variable "apl_values_env" {
  description = "Subdirectory under apl-values/ for this cluster (lab, staging, primary, secondary). The apl-core values file is rendered from apl-values/<env>/values.yaml; the Argo CD root path is also set to this subdir."
  type        = string
}

variable "cluster_name" {
  description = "Logical cluster name injected into apl-core values as cluster.name (Istio Gateway hostnames, Argo CD context, Console banner). For a spec instance this matches cluster.bootstrap.name, which `llz render` has already written into the committed values.yaml."
  type        = string
}

variable "cluster_domain" {
  description = "Base DNS domain for this cluster (e.g. lab.<domain>, <domain> for primary). Injected as cluster.domainSuffix + dns.domainFilters; also read from these tfvars by `llz ci resolve-harbor-url` to derive harbor.<cluster_domain>. ExternalDNS uses it; cert-manager Let's Encrypt covers its subdomains. Spec instances: cluster.bootstrap.domainSuffix."
  type        = string
}

variable "apl_values_repo_url" {
  description = "HTTPS URL of the GitOps repo that holds apl-values/ and manifest/ subtrees. **HTTPS is required** by apl-core's values schema (otomi.git.repoUrl pattern `^https?://.+`). A host that requires per-cluster node-IP allowlisting for HTTPS cannot satisfy LKE-E, so the values tree must be mirrored to a public-CA HTTPS-reachable host (GitHub.com, GitLab.com, or an internal HTTPS mirror). Example: https://github.com/<org>/platform-apl-values.git"
  type        = string
}

variable "apl_values_repo_revision" {
  description = "Branch or tag the in-cluster Argo CD tracks in the values repo."
  type        = string
  default     = "main"
}

variable "apl_values_repo_username" {
  description = "Username for HTTPS Git basic-auth against the values repo. With a GitHub fine-grained PAT the username is ignored by the server, so the conventional 'x-access-token' is used (any non-empty string works). Supply via TF_VAR_apl_values_repo_username to override."
  type        = string
  default     = "x-access-token"
}

variable "apl_values_repo_token" {
  description = "Fine-grained GitHub PAT used as the HTTPS Git password for the values repo (apl-core's otomi.git.password) and the platform-apps repo (kubectl_manifest.argocd_apps_repo). Needs `Contents: write` on the instance repo because apl-operator PUSHES its rendered values tree there during bootstrap. Supply via TF_VAR_apl_values_repo_token (sourced from secrets.APL_VALUES_REPO_TOKEN). Replaces the former in-cluster Gitea admin password (random_password.gitops_repo_password) and the platform-apps SSH deploy key (apps_repo_ssh_key), both retired when Gitea was obsoleted."
  type        = string
  sensitive   = true
}

variable "linode_dns_token" {
  description = "Linode API token scoped to DNS zone write only. Injected as dns.provider.linode.apiToken so ExternalDNS + cert-manager DNS-01 can manage records under cluster.domainSuffix. Supply via TF_VAR_linode_dns_token. In CI, sourced from secrets.LINODE_DNS_TOKEN with a non-blocking placeholder fallback when the secret isn't provisioned on the env — the placeholder satisfies apl-core's string-type schema requirement so the cluster bootstraps, but ExternalDNS and cert-manager DNS-01 fail to authenticate at runtime until a real token is provisioned."
  type        = string
  sensitive   = true
}

variable "apps_repo_revision" {
  description = "Branch, tag, or commit of the instance repo (https://github.com/<org>/<instance-repo>.git) that the TF-managed bootstrap ArgoCD Application (apl-values/<env>/manifest/) targets. Distinct from apl_values_repo_revision (apl-core's own otomi.git values fetch); the manifest tree can run a feature branch while apl-core tracks main. Defaults to 'main'; override via TF_VAR_apps_repo_revision (typically sourced from vars/secrets.APPS_REPO_REVISION, falling back to 'main') when testing feature branches before merge."
  type        = string
  default     = "main"
}

variable "loki_admin_password" {
  description = "Admin password for the Loki gateway's HTTP basic auth. Required by apl-core's apps.loki schema when loki.enabled=true; rendered into apl-values/<env>/values.yaml as apps.loki.adminPassword. Supply via TF_VAR_loki_admin_password (sourced from secrets.LOKI_ADMIN_PASSWORD). When empty (first apply, before the secret exists) cluster-bootstrap generates one via random_password.loki_admin and the llz-terraform workflow persists it to the infra-<region> environment as LOKI_ADMIN_PASSWORD; later runs pass the stored value back in. NOTE: not yet on the ESO+OpenBao rotation lifecycle the other support-plane creds use — see docs/secrets.md (Known limitation — Loki admin password); moving it there is a tracked follow-up."
  type        = string
  sensitive   = true
  default     = ""
}

variable "destroying" {
  description = "Set true (TF_VAR_destroying=true) only on the teardown path. Gates data.kubernetes_service.coredns off so `terraform destroy` doesn't refresh that cluster-API read while the LKE cluster is being reaped in the same run — the read would time out (dial :6443 i/o timeout) and fail the destroy. The data source is apply-only (it just feeds the rendered Loki gateway resolver), so skipping it on destroy is safe; the Kyverno loki-gateway-resolver policy is the runtime backstop. Defaults false so the apply path is unaffected and no apply job needs to set it."
  type        = bool
  default     = false
}

variable "linode_token" {
  description = "Linode account API token. Consumed by the destroy-time provisioner on null_resource.cleanup_platform_volumes_on_destroy to sweep any Block Storage Volume tagged `block-storage` that's left unattached after PVC reap — this is the orphan-prevention that keeps account-quota exhaustion from blocking the next bootstrap. Same value as the cluster module's linode_token; in CI both are wired from secrets.LINODE_API_TOKEN via TF_VAR_linode_token."
  type        = string
  sensitive   = true
}

variable "openbao_secrets_write_token" {
  description = "GitHub PAT with `repo` + `secrets:write` scope. Consumed by null_resource.clear_openbao_secrets_on_destroy to delete OPENBAO_ROOT_TOKEN (and downstream OPENBAO_UNSEAL_KEY_{1,2,3}, OPENBAO_APPROLE_SECRET_ID_<REGION>, HARBOR_ROBOT_NAME, HARBOR_PASSWORD) from the `infra-<region>` environment on cluster-bootstrap destroy. After a cluster destroy these are all bound to an OpenBao / Harbor instance that no longer exists; leaving them set lets stale-credential failures latch onto the next bootstrap (Configure OpenBao 403 permission denied). Optional: empty default = no-op + log a warning, operator clears manually."
  type        = string
  sensitive   = true
  default     = ""
}

variable "ghcr_username" {
  description = "GitHub username that owns the GHCR read token (ghcr_token). GHCR OCI auth needs a real account name (the PAT owner), not a placeholder. Supply via TF_VAR_ghcr_username (sourced from vars.GHCR_USERNAME). Empty default = the ArgoCD GHCR repo Secret is skipped (kept optional so plan/destroy work without it)."
  type        = string
  default     = ""
}

variable "ghcr_token" {
  description = "OPTIONAL GitHub token for GHCR. The first-party OCI Helm charts under ghcr.io/<@ upstream_org @>/charts are PUBLIC, so ArgoCD pulls them anonymously and this is normally left empty. Set it only for (a) a private fork that keeps its charts private, or (b) the optional Akamai-internal firewall-controller-internal image (see docs/consume-lke-landing-zone-internal.md). When set, supply via TF_VAR_ghcr_token (sourced from secrets.GHCR_READ_TOKEN) and pair with var.ghcr_username; a classic PAT with `read:packages` is simplest. Empty default = no GHCR Secret created (the public-charts path)."
  type        = string
  sensitive   = true
  default     = ""
}
