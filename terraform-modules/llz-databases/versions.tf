terraform {
  required_version = ">= 1.5.0"

  required_providers {
    linode = {
      source = "linode/linode"
      # linode_database_postgresql_v2 (with the VPC-attaching private_network
      # ATTRIBUTE — it is a nested attribute, not a block) requires a v2-era
      # provider, hence ~> 4.3, the same CONSTRAINT the cluster and object-storage
      # roots carry — and the same one the generated roots in
      # tools/internal/shared/tfroots/roots/*/versions.tf declare. Terraform
      # INTERSECTS a root's constraint with its module's, so those two must move
      # together or the intersection is empty and `tofu init` cannot resolve the
      # provider at all. provider-constraint-agreement enforces that.
      #
      # That is a shared constraint, NOT a shared version: what an instance actually
      # runs is whatever each root's .terraform.lock.hcl pins, and those are already
      # per-root and already differ.
      # This constraint does not make an instance pin ONE linode provider version,
      # and nothing here does.
      #
      # The databases root has NO tracked lock file (neither does vpc), so its
      # provider floats within this constraint at every init and it is invisible to
      # `make sbom-terraform`, which builds release provenance by parsing every
      # .terraform.lock.hcl. Adding one is a deliberate supply-chain choice — which
      # version to pin — so it is left to the branch owner rather than fixed here.
      version = "~> 4.3"
    }
  }
}
