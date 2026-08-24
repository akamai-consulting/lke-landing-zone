terraform {
  required_version = ">= 1.5.0"

  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 4.3"
    }
    time = {
      source  = "hashicorp/time"
      version = "~> 0.12"
    }
  }
}
