# One shared, region-scoped VPC, referenced by environments via the spec's
# cluster.network.vpc (landingzone.yaml spec.networks). A Linode VPC is a
# container only — it carries no CIDR; each attaching cluster creates its own
# subnet inside it (terraform-modules/llz-cluster, var.vpc_id). The VPC label is
# the network name, which the cluster root looks up to attach.
#
# This root is applied per-network (state key vpc/<name>) BEFORE the clusters that
# attach (see the llz-terraform workflow's apply-vpc → apply-cluster ordering).
# Applies are idempotent + serialized so the first cluster in a network creates
# the VPC and later ones attach. Instances using only dedicated VPCs never run it.
resource "linode_vpc" "this" {
  label       = var.vpc_label
  region      = var.region
  description = "Shared LLZ VPC ${var.vpc_label}"
}
