terraform {
  required_version = ">= 1.5.0"

  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 4.3"
    }
  }
}

# obj_use_temp_keys: the openbao-backup, velero and cnpg buckets are VERSIONED and carry
# a lifecycle rule (llz-object-storage), and both are S3 calls that need an
# object-storage key, not just the Linode API token. With this set the provider
# mints a short-lived key from LINODE_TOKEN for each operation that needs one and
# deletes it afterwards — so no key lives in state or in a tfvars, which is the
# property this root has kept since the TF-managed keys were removed.
provider "linode" {
  obj_use_temp_keys = true
}
