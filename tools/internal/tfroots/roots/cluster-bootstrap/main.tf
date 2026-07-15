# cluster-bootstrap — the in-cluster bootstrap layer that runs after the LKE
# cluster is up but before any application workload reaches a working state.
#
# CONVERGENCE CONTRACT — see docs/architecture/convergence-contract.md.
#
# In short:
#   - This module's `terraform apply` returning success means "every TF
#     resource here was placed AND the apl-operator pipeline reached a state
#     where the next step in the bootstrap chain can run". The deep-
#     convergence claim ("platform-bootstrap Application is Synced+Healthy with
#     only operator-deferred items still PENDING") is enforced by the
#     terraform.yml workflow's ``llz ci converge`` step that runs AFTER
#     this apply returns. Together they make "the workflow passed" mean
#     "the cluster works", not "every command happened to return 0".
#   - The single readiness gate `null_resource.apl_pipeline_ready` (now in the
#     llz-cluster-bootstrap module this root calls) replaces ~140 lines of
#     imperative polling that used to live across
#     `null_resource.wait_for_argo_application_crd` and
#     `null_resource.wait_for_kyverno_crd`. Both real readiness signals
#     (Argo CD application-controller StatefulSet Ready, Kyverno admission-
#     controller Deployment Available) instead of CRD-existence polls +
#     webhook-config polls + svc-endpoints polls.
#   - This root is a THIN wrapper: it reads the instance's kubeconfig (cluster
#     remote state), renders the apl-core values (templatefile over the committed
#     apl-values/<env>/values.yaml), and passes them into the shared
#     terraform-modules/llz-cluster-bootstrap module, which holds all the
#     provisioning resources. When adding new resources, add them to that module
#     and depend on `null_resource.apl_pipeline_ready` if you need argo/kyverno
#     up; do NOT add new polling `null_resource`s — extend `apl_pipeline_ready`.
#
# ── Read cluster outputs (kubeconfig + IDs) from the cluster workspace ────────
# This workspace is one-way coupled to the cluster workspace: it reads outputs
# but does not write anything that the cluster workspace depends on.  The
# kubeconfig is consumed directly by the providers (see providers.tf) — it is
# never written to disk.
locals {
  # Linode Object Storage speaks S3 but fails standard AWS validation — every
  # terraform_remote_state read needs the same skip_* flags + path-style
  # addressing (mirrors backend.tf). Merge these into each data source's config.
  remote_state_s3_defaults = {
    skip_credentials_validation = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
    skip_requesting_account_id  = true
    skip_s3_checksum            = true
    use_path_style              = true
  }
}

data "terraform_remote_state" "cluster" {
  backend = "s3"
  config = merge(local.remote_state_s3_defaults, {
    bucket = var.tf_state_bucket
    key    = "cluster/${var.deployment}/terraform.tfstate"
    region = "us-east-1"
  })
}

# ── Akamai App Platform (apl-core) ────────────────────────────────────────────
# Installs apl-core via its top-level Helm chart. The chart bootstraps the
# apl-operator, which in turn drives the helmfile pipeline that installs ~40
# components (Istio, Argo CD, Keycloak, cert-manager, ESO, CNPG,
# Sealed Secrets, kube-prometheus-stack, Grafana, Loki, OTel Operator,
# Harbor, Kyverno, Trivy, ExternalDNS — apl-core's bundled Tekton AND Gitea
# charts are disabled in our per-env values.yaml; cert-automation runs on Argo
# Workflows + Events, and the values store is the external GitHub repo over
# HTTPS, not the in-cluster Gitea) and then hands off to Argo CD
# for continuous reconciliation from the values repo configured below.
#
# The values file is rendered per environment from apl-values/<env>/values.yaml
# at the repo root. We pass it through templatefile so the values-repo HTTPS
# PAT and the Linode DNS token (both held in TF state as sensitive variables)
# can be injected into otomi.git.password and dns.provider.linode.apiToken
# without leaking them into the file on disk.
# Render once into a local; helm_release consumes it and the local_file dump
# below writes the exact same bytes to disk for diagnostic inspection. If
# apl-operator does something unexpected with `_rawValues` or any other
# templated field, the rendered file is the first artifact to look at — it
# tells you what apl-operator actually saw, not what you intended.
#
# apps.loki.adminPassword is REQUIRED by apl-core's values schema when loki is
# enabled. On v6 it is an x-secret with a generator, but that generator only runs
# inside otomi's OWN bootstrap — our raw `helm install apl/apl` validates the
# schema FIRST (before apl-operator can install and generate it), so we must
# supply a value or the install fails "apps.loki: adminPassword is required".
# Generate it here; random_password is stable in TF state, so it does not churn
# across reconciles. Nothing on the landing-zone side consumes it — it is loki's
# internal gateway<->grafana basic-auth credential, owned by apl-core.
# (v6-GA correction: an earlier migration commit dropped this on the assumption
# apl-core self-generates it; the e2e helm install proved the schema requires it.)
resource "random_password" "loki_admin" {
  length  = 20
  special = false # match apl-core's `randAlphaNum 20` generator (nginx-safe charset)
}

# Cluster DNS Service ClusterIP, for the loki-gateway nginx `resolver`. The
# grafana/loki gateway templates `resolver <dnsService>.kube-system.svc...;` as a
# HOSTNAME, which nginx can't use as a nameserver → it crashloops ("host not
# found in resolver"). nginx needs the DNS Service's IP, which varies per cluster
# (the service CIDR is LKE-E-assigned). Read it here and render it into the loki
# chart values (apps.loki._rawValues.gateway.nginxConfig.resolver) so the gateway
# ships a usable resolver from the start — no dependency on the Kyverno mutation
# webhook being up at cm-create time (the failure mode that crashlooped the
# gateway when Kyverno lagged). On LKE-E the cluster DNS Service is `coredns` in
# kube-system; cluster-bootstrap already has working cluster access, so this read
# is safe. NOTE: this rendered value is now the SOLE mechanism — the Kyverno
# loki-gateway-resolver policy that once backstopped an empty value was RETIRED
# (see the retired-policy note further down, and apl-values/_shared/values.yaml).
# So an empty value is NOT self-healing: it ships a crashlooping gateway. The read
# must succeed on apply (it does on any reachable cluster; the only place it's
# skipped is the destroy path, via the count guard below).
#
# count guards the destroy path: this is a data source, so `terraform destroy`
# still refreshes it even after the teardown's "Untrack cluster-bootstrap
# resources" step has `state rm`'d every managed in-cluster resource (state rm
# cannot drop a data source). On a CASE B teardown (cluster being reaped in the
# same run) the API server is unreachable and the read times out (dial :6443 i/o
# timeout), failing the whole destroy. The value is apply-only (it only feeds the
# rendered Loki values below), so skip the read entirely when destroying.
data "kubernetes_service" "coredns" {
  count = var.destroying ? 0 : 1
  metadata {
    name      = "coredns"
    namespace = "kube-system"
  }
}

locals {
  # Fills the ONLY placeholders left in the committed values.yaml: the runtime
  # secrets (loki admin password, values-repo PAT, DNS token) and the live coredns
  # ClusterIP. Everything else — cluster identity, the Loki/Harbor object-store
  # wiring (bucket names + S3 endpoint/region), and the otomi.git repo coordinates —
  # is resolved into the committed values.yaml by `llz render` from the LandingZone
  # spec (see tools/internal/clusterspec/values.go), so a landingzone.yaml spec is
  # REQUIRED: there is no non-spec render path. templatefile() errors if the file
  # references a var absent from this map, which catches a stale placeholder the
  # render should have resolved.
  apl_rendered_values = templatefile(
    "${path.module}/../../apl-values/${var.apl_values_env}/values.yaml",
    {
      apl_values_repo_password = var.apl_values_repo_token
      linode_dns_token         = var.linode_dns_token
      coredns_cluster_ip       = try(data.kubernetes_service.coredns[0].spec[0].cluster_ip, "")
      loki_admin_password      = random_password.loki_admin.result
    }
  )

  # Extract `data.revision: <value>` from the env-revision-configmap.yaml in
  # the cloned tree. The regex tolerates leading/trailing whitespace, optional
  # single/double quotes around the value, and YAML's loose spacing rules. If
  # the file is reformatted in a way this can't parse, the precondition below
  # fails with a clear error rather than a silent mismatch.
  env_revision_configmap_content = data.local_file.env_revision_configmap.content
  env_revision_in_configmap      = trimspace(regex("revision:\\s*['\"]?([^\\s'\"#]+)['\"]?", local.env_revision_configmap_content)[0])
}

# Read the env-revision-configmap file from the cloned repo. The file is
# `config.kubernetes.io/local-config: true` so kustomize uses its data.revision
# as the substitution source for every in-repo child Argo CD Application's
# targetRevision at build time. If this file's revision drifts from
# var.apps_repo_revision, Argo will render the bootstrap App at one
# branch (set via templatefile in apl_rendered_values above) and child Apps
# at another — silently pulling stale code that may not match the bootstrap
# App's view of the world. The precondition below catches that mismatch at
# plan time.
data "local_file" "env_revision_configmap" {
  filename = "${path.module}/../../apl-values/${var.apl_values_env}/manifest/env-revision-configmap.yaml"
}

# ── cluster-bootstrap provisioning module ────────────────────────────────────
# The in-cluster provisioning resources (the apl-core Helm release, the argocd/
# ghcr/bootstrap kubectl_manifests, and the wait/kyverno null_resources) live in
# the reusable llz-cluster-bootstrap module so a sibling system team can run the
# same bootstrap. This root stays THIN: it reads the instance-relative files (the
# apl-values templatefile + env-revision configmap, above), owns the providers
# (configured from the cluster workspace's remote-state kubeconfig — see
# providers.tf), and passes both down. Mirrors object-storage/ ↔
# terraform-modules/llz-object-storage.
module "cluster_bootstrap" {
  # checkov:skip=CKV_TF_1: First-party module sources pin to immutable-by-convention
  # SemVer tags (terraform-modules/RELEASING.md — tags are never moved), which are the
  # human-readable version contract; a raw commit SHA here would defeat that scheme.
  source = "git::ssh://git@github.com/<@ upstream_org @>/lke-landing-zone.git//terraform-modules/llz-cluster-bootstrap?ref=<@ llz_version @>"
  # source = "../../terraform-modules/llz-cluster-bootstrap"

  # helm/kubernetes/kubectl are configured in providers.tf from the cluster
  # workspace's remote-state kubeconfig; hand them to the module explicitly.
  providers = {
    helm       = helm
    kubernetes = kubernetes
    kubectl    = kubectl
  }

  # Instance-relative values the root resolves and the module consumes.
  apl_rendered_values       = local.apl_rendered_values
  env_revision_in_configmap = local.env_revision_in_configmap
  kubeconfig_raw            = local.kubeconfig_raw

  apl_chart_version  = var.apl_chart_version
  apl_values_env     = var.apl_values_env
  apps_repo_revision = var.apps_repo_revision
  ghcr_username      = var.ghcr_username
  ghcr_token         = var.ghcr_token

  # Identity tokens the module CANNOT carry itself: it is git-fetched from the
  # template repo, which copier/tfroots never render — so the bootstrap App's
  # repoURL + the GHCR charts URL take these as inputs from this (rendered) root.
  instance_repo = "<@ instance_repo @>"
  upstream_org  = "<@ upstream_org @>"
}

# ── Destroy-time cleanup relocated from TF to the CI destroy job ──────────────
# Three destroy-time `null_resource` provisioners used to run here: an
# orphan-Volume sweep (already a no-op — the real sweep is the DESTROY Cluster
# job's `llz ci reap-volumes`), the namespace-finalizer unwedge, and the
# OpenBao/Harbor GH-env-secret cleanup. They are now explicit steps in
# .github/workflows/llz-terraform.yml's destroy-cluster-bootstrap job
# (`llz ci destroy-unwedge --region` and `llz ci clear-cluster-secrets`), so the
# logic is unit-tested Go rather than inline-bash local-exec heredocs and TF
# holds no destroy-time provisioners that could fire against a live cluster.
#
# `removed` blocks (with `lifecycle { destroy = false }`) drop the resources from
# any existing state WITHOUT running their destroy provisioners — the safe way to
# retire a resource carrying a `when = destroy` provisioner. For a fresh instance
# with no prior state they are harmless no-ops; they can be deleted once every
# deployment's state has reconciled past this change.
removed {
  from = null_resource.cleanup_platform_volumes_on_destroy
  lifecycle {
    destroy = false
  }
}

removed {
  from = null_resource.unwedge_namespace_finalizers_on_destroy
  lifecycle {
    destroy = false
  }
}

removed {
  from = null_resource.clear_openbao_secrets_on_destroy
  lifecycle {
    destroy = false
  }
}
