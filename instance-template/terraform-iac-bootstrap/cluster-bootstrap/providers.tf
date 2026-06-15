# Providers are configured directly from the cluster's kubeconfig, decoded
# from the cluster workspace's remote-state output.  This value is known at
# plan time (terraform_remote_state is read during refresh), so there is no
# chicken-and-egg between provider configuration and a resource that writes a
# kubeconfig file — we never touch the local filesystem.
#
# RESILIENCE TO AN EMPTY CLUSTER STATE — the cluster workspace's remote state
# is EMPTY (outputs is an object with no attributes) whenever this workspace is
# torn down AFTER the cluster has already been deleted: a re-run of a partial
# destroy, or a `module == all` destroy where the cluster was reaped first.
# A bare `.kubeconfig_raw` reference then hard-fails at plan time
# ("Unsupported attribute"), which blocks even `terraform plan -destroy` and
# leaves this workspace's state un-cleanable. `try()` degrades each lookup to a
# harmless fallback so the destroy plan proceeds; the providers point at an
# unreachable dummy endpoint (the cluster is gone) and `terraform destroy`
# simply drops the orphaned resources from state. On a LIVE cluster every
# try() resolves to the real value, so this is a no-op for apply.
#
# `local.kubeconfig_raw` is the single source every other reference in this
# module funnels through (see main.tf), so the cluster-gone fallback is applied
# consistently and the destroy-time provisioner triggers stay byte-stable.

locals {
  # The cluster workspace's remote-state output is one of:
  #   absent  -> cluster workspace already destroyed (try() falls back to "")
  #   null    -> cluster CREATED but never initialized (control plane never came
  #              up; e.g. nodes failed to provision) — output present, no kubeconfig
  #   string  -> a live cluster
  # Normalize the first two to "" so "no usable kubeconfig" is represented
  # IDENTICALLY everywhere it is tested: the unreachable-endpoint fallback below
  # AND the destroy untrack step's `kubeconfig_raw == ""` cluster-gone check
  # (`llz ci tf-untrack`). Before this, a null slipped
  # through as "live": untrack picked CASE A, left the kubectl_manifest resources
  # in state, and `plan -destroy`'s refresh dialed the cluster-already-destroyed
  # .invalid sentinel host -> DNS "no such host".
  _kubeconfig_out = try(data.terraform_remote_state.cluster.outputs.kubeconfig_raw, "")
  kubeconfig_raw  = local._kubeconfig_out == null ? "" : local._kubeconfig_out
  # yamldecode("" | garbage) -> null; try() keeps a malformed kubeconfig from
  # hard-failing plan. The kube_* locals below then fall back to the sentinel.
  _kubeconfig = try(yamldecode(local.kubeconfig_raw), null)
  _kc_cluster = try(local._kubeconfig.clusters[0].cluster, null)
  _kc_user    = try(local._kubeconfig.users[0].user, null)

  kube_host  = try(local._kc_cluster.server, "https://cluster-already-destroyed.invalid")
  kube_ca    = try(base64decode(local._kc_cluster["certificate-authority-data"]), "")
  kube_token = try(local._kc_user.token, "")
}

provider "helm" {
  kubernetes {
    host                   = local.kube_host
    cluster_ca_certificate = local.kube_ca
    token                  = local.kube_token
  }
}

provider "kubernetes" {
  host                   = local.kube_host
  cluster_ca_certificate = local.kube_ca
  token                  = local.kube_token
}

provider "kubectl" {
  host                   = local.kube_host
  cluster_ca_certificate = local.kube_ca
  token                  = local.kube_token
  load_config_file       = false
}
