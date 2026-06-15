variable "openbao_address" {
  description = "OpenBao API address. Use https://localhost:8200 with a port-forward for local runs, or https://platform-openbao.openbao.svc.cluster.local:8200 from inside the cluster."
  type        = string
}

variable "openbao_token" {
  description = "Root or admin token. Use bao operator generate-root to mint a new one; revoke it after apply."
  type        = string
  sensitive   = true
}

variable "openbao_skip_tls_verify" {
  description = "Skip TLS certificate verification. Set true when connecting via port-forward to avoid SNI mismatch against the internal CA."
  type        = bool
  default     = false
}

variable "openbao_ca_cert_file" {
  description = "Path to the CA certificate file used to verify OpenBao's TLS certificate. Leave empty to use system CAs or when skip_tls_verify is true."
  type        = string
  default     = ""
}

variable "kubernetes_host" {
  description = "Kubernetes API server URL used by OpenBao's Kubernetes auth method for token validation. Should be the in-cluster address visible from OpenBao pods."
  type        = string
  default     = "https://kubernetes.default.svc:443"
}
