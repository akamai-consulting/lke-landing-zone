# ── OpenBao / Kubernetes wiring ───────────────────────────────────────────────

variable "kubernetes_host" {
  description = "Kubernetes API server URL used by OpenBao's Kubernetes auth method for token validation. Should be the in-cluster address visible from OpenBao pods."
  type        = string
  default     = "https://kubernetes.default.svc:443"
}

variable "kv_path" {
  description = "Mount path for the KV v2 secret engine."
  type        = string
  default     = "secret"
}

# ── CI AppRole (consumed by ESO ClusterSecretStore + GitHub Actions) ──────────

variable "ci_role_name" {
  description = "Name of the CI AppRole and its read policy. Org/deployment identity — a sibling team picks its own. The approle-rotator policy is templated against this name."
  type        = string
  default     = "platform-ci"
}

variable "ci_role_id" {
  description = "Pinned role_id for the CI AppRole. MUST match the roleId in the ESO ClusterSecretStore. Defaults to ci_role_name when left empty."
  type        = string
  default     = ""
}

variable "ci_read_paths" {
  description = "KV v2 sub-paths (below kv_path) the CI AppRole may read. Each entry generates a `<kv_path>/data/<p>` read grant and a `<kv_path>/metadata/<p>` read+list grant. This is the org-specific secret tree — a sibling team supplies its own list."
  type        = list(string)
  default = [
    "approle/rotation-secrets",
    "cert-automation/github-token",
    "certmanager/dns01",
    "grafana/admin",
    "harbor/admin",
    "harbor/docker-config",
    "harbor/pull-robot",
    "harbor/registry-s3",
    "harbor/robot",
    "infra/github-dispatch-token",
    "loki/object-store",
    "otel/ingress",
  ]
}

variable "ci_token_ttl" {
  description = "Token TTL for the CI AppRole."
  type        = string
  default     = "15m"
}

variable "ci_token_max_ttl" {
  description = "Token max TTL for the CI AppRole."
  type        = string
  default     = "30m"
}

variable "ci_secret_id_ttl" {
  description = "secret_id TTL for the CI AppRole. Default 2208h (92 days) outlasts the quarterly rotation interval."
  type        = string
  default     = "2208h"
}

# ── AppRole-rotator Kubernetes auth role ──────────────────────────────────────

variable "rotator_role_name" {
  description = "Name of the Kubernetes auth role + policy that manages secret_ids for the CI AppRole."
  type        = string
  default     = "approle-rotator"
}

variable "rotator_service_account_names" {
  description = "Service accounts bound to the rotator Kubernetes auth role."
  type        = list(string)
  default     = ["approle-rotator"]
}

variable "rotator_service_account_namespaces" {
  description = "Namespaces the rotator service accounts live in."
  type        = list(string)
  default     = ["llz-openbao"]
}

variable "rotator_token_ttl" {
  description = "Token TTL for the rotator Kubernetes auth role."
  type        = string
  default     = "15m"
}
