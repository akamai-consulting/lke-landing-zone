terraform {
  required_version = ">= 1.5.0"

  required_providers {
    vault = {
      # hashicorp/vault is API-compatible with OpenBao at the OSS feature level.
      source  = "hashicorp/vault"
      version = "~> 4.0"
    }
  }
}
