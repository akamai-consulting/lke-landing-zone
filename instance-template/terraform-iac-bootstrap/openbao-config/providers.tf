provider "vault" {
  address         = var.openbao_address
  token           = var.openbao_token
  skip_tls_verify = var.openbao_skip_tls_verify

  # ca_cert_file is only set when a path is provided; omitting it lets the
  # provider fall back to system CAs (correct for the port-forward + skip_tls path).
  ca_cert_file = var.openbao_ca_cert_file != "" ? var.openbao_ca_cert_file : null
}
