terraform {
  required_version = ">= 1.5.0"

  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 3.11"
    }
    # (the `time` provider was removed with the TF-managed keys and their
    # time_rotating rotation clock — the in-cluster linodeCredRotator owns
    # rotation; see main.tf's "Access keys" note.)
  }
}
