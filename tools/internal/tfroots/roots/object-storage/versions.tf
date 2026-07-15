terraform {
  required_version = ">= 1.5.0"

  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 3.11"
    }
    # Drives the 120-day Object Storage key rotation SLA declaratively
    # (LKE Secrets Rotation Guidelines — revoke bucket access keys after
    # 120 days). Linode OBJ keys have no native expiry, so rotation is a
    # forced destroy+recreate keyed off time_rotating; see main.tf.
    time = {
      source  = "hashicorp/time"
      version = "~> 0.12"
    }
  }
}
