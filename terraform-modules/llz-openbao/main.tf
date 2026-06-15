# Reusable OpenBao/Vault bootstrap: KV v2 engine, AppRole + Kubernetes auth, a
# read policy for CI/ESO, and the AppRole-rotation wiring. Extracted from the
# openbao-config/ root per docs/templatization-plan.md §6. The org-specific
# pieces — role names, role_id, and the secret tree the CI policy grants — are
# variables (ci_role_name / ci_role_id / ci_read_paths) so a sibling system team
# brings its own without editing HCL.

locals {
  # role_id defaults to the role name when not pinned explicitly.
  ci_role_id = var.ci_role_id != "" ? var.ci_role_id : var.ci_role_name

  # Generate the CI read policy from ci_read_paths: each sub-path gets a
  # read grant on its KV v2 data path and a read+list grant on its metadata
  # path. Templating (not a hard-coded heredoc) is what makes this reusable.
  _ci_data_grants = [
    for p in var.ci_read_paths :
    format("path \"%s/data/%s\" { capabilities = [\"read\"] }", var.kv_path, p)
  ]
  _ci_metadata_grants = [
    for p in var.ci_read_paths :
    format("path \"%s/metadata/%s\" { capabilities = [\"read\", \"list\"] }", var.kv_path, p)
  ]
  ci_policy = join("\n", concat(local._ci_data_grants, [""], local._ci_metadata_grants))

  rotator_policy = <<-EOT
    path "auth/approle/role/${var.ci_role_name}/secret-id" {
      capabilities = ["create", "update", "list"]
    }
    path "auth/approle/role/${var.ci_role_name}/secret-id-accessor/destroy" {
      capabilities = ["update"]
    }
    path "auth/approle/role/${var.ci_role_name}/role-id" {
      capabilities = ["read"]
    }
  EOT
}

# ── Secret engine ────────────────────────────────────────────────────────────

resource "vault_mount" "kv" {
  path        = var.kv_path
  type        = "kv"
  options     = { version = "2" }
  description = "KV v2 secret store for the platform stack"
}

# ── Auth methods ──────────────────────────────────────────────────────────────

resource "vault_auth_backend" "approle" {
  type = "approle"
  path = "approle"
}

resource "vault_auth_backend" "kubernetes" {
  type = "kubernetes"
  path = "kubernetes"
}

# Uses in-pod service account token/CA auto-discovery when OpenBao runs inside
# the cluster. kubernetes_host is the only required field in that case.
resource "vault_kubernetes_auth_backend_config" "default" {
  backend         = vault_auth_backend.kubernetes.path
  kubernetes_host = var.kubernetes_host
}

# ── Policies ──────────────────────────────────────────────────────────────────

# Read-only access to the KV v2 paths consumed by ESO and CI. Shared by ESO
# (ClusterSecretStore) and GitHub Actions CI. Generated from var.ci_read_paths.
resource "vault_policy" "ci" {
  name   = var.ci_role_name
  policy = local.ci_policy
}

# Manages secret_ids for the CI AppRole. Used by the approle-rotation
# CronWorkflow via Kubernetes auth.
resource "vault_policy" "rotator" {
  name   = var.rotator_role_name
  policy = local.rotator_policy
}

# ── AppRole role ──────────────────────────────────────────────────────────────

resource "vault_approle_auth_backend_role" "ci" {
  backend   = vault_auth_backend.approle.path
  role_name = var.ci_role_name

  # Pinned role_id — must match the roleId field in the ESO ClusterSecretStore.
  role_id = local.ci_role_id

  token_policies = [vault_policy.ci.name]
  token_ttl      = var.ci_token_ttl
  token_max_ttl  = var.ci_token_max_ttl

  # Default 2208h (92 days) — matches the quarterly rotation schedule and ensures
  # the credential outlasts the Jul→Oct and Oct→Jan intervals (92 days each).
  secret_id_ttl = var.ci_secret_id_ttl
}

# ── Kubernetes auth role ───────────────────────────────────────────────────────

resource "vault_kubernetes_auth_backend_role" "rotator" {
  backend   = vault_auth_backend.kubernetes.path
  role_name = var.rotator_role_name

  bound_service_account_names      = var.rotator_service_account_names
  bound_service_account_namespaces = var.rotator_service_account_namespaces

  token_policies = [vault_policy.rotator.name]
  token_ttl      = var.rotator_token_ttl
}
