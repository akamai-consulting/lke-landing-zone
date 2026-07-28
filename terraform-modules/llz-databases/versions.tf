terraform {
  required_version = ">= 1.5.0"

  required_providers {
    linode = {
      source = "linode/linode"
      # linode_database_postgresql_v2 (with the VPC-attaching private_network
      # block) requires a v2-era provider; ~> 3.11 matches the cluster/
      # object-storage roots so an instance pins one linode provider version.
      version = "~> 3.11"
    }
  }
}
