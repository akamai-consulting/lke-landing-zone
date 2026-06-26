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
#   - The single readiness gate `null_resource.apl_pipeline_ready` (defined
#     below) replaces ~140 lines of imperative polling that used to live
#     across `null_resource.wait_for_argo_application_crd` and
#     `null_resource.wait_for_kyverno_crd`. Both real readiness signals
#     (Argo CD application-controller StatefulSet Ready, Kyverno admission-
#     controller Deployment Available) instead of CRD-existence polls +
#     webhook-config polls + svc-endpoints polls.
#   - When adding new resources to this module: depend on
#     `null_resource.apl_pipeline_ready` if you need argo/kyverno to be up.
#     Do NOT add new polling `null_resource`s — extend `apl_pipeline_ready`
#     instead.
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
# Loki gateway HTTP basic-auth admin password (apl-core's apps.loki schema
# REQUIRES adminPassword when loki is enabled). var.loki_admin_password is now
# always supplied — the llz-terraform workflow runs `llz ci ensure-env-secret`
# BEFORE this apply, which generates+persists the infra-<region> LOKI_ADMIN_PASSWORD
# secret on first run and exports it as TF_VAR_loki_admin_password. cluster-bootstrap
# no longer generates it (the former random_password.loki_admin) or outputs it for a
# post-apply stash; it is a plain consumed input, kept out of TF state's secret set.

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
  # Object-store wiring, derived from var.obj_cluster + var.deployment instead of a
  # cross-workspace `terraform_remote_state` read of the object-storage workspace
  # (dropping that coupling — object-storage no longer has to be applied first for
  # cluster-bootstrap to plan). The bucket labels are deterministic: they mirror the
  # llz-object-storage module's "${label_prefix}-<bucket>-${region_suffix}" naming,
  # where label_prefix defaults to "platform" and region_suffix is the deployment
  # (== var.deployment). obj_label_prefix MUST stay in lockstep with
  # terraform-modules/llz-object-storage/variables.tf's label_prefix default.
  #
  # s3_endpoint is "https://<obj_cluster>.linodeobjects.com": Loki's chart wants the
  # bare host for `s3.endpoint`; Harbor's registry wants the full URL for
  # `regionendpoint`. `s3_region` is the OBJ cluster id (e.g. us-ord-1), used by both.
  obj_label_prefix = "platform"
  obj_s3_url       = "https://${var.obj_cluster}.linodeobjects.com"
  loki_buckets = {
    chunks = "${local.obj_label_prefix}-loki-chunks-${var.deployment}"
    ruler  = "${local.obj_label_prefix}-loki-ruler-${var.deployment}"
    admin  = "${local.obj_label_prefix}-loki-admin-${var.deployment}"
  }
  harbor_bucket = "${local.obj_label_prefix}-harbor-registry-${var.deployment}"
  s3_host       = replace(local.obj_s3_url, "https://", "")
  s3_region     = replace(local.s3_host, ".linodeobjects.com", "")

  # Fills the values.yaml placeholders: the cluster identity (cluster_name/
  # cluster_domain) + the secrets/infra outputs (repo creds, dns token, loki/harbor
  # object-store, coredns IP). For a SPEC instance, `llz render` has already written
  # the identity literals into the committed values.yaml from the LandingZone spec,
  # so those two are resolved before this runs and the vars here are a no-op for it;
  # for a non-spec instance they ARE the identity render path.
  apl_rendered_values = templatefile(
    "${path.module}/../../apl-values/${var.apl_values_env}/values.yaml",
    {
      cluster_name             = var.cluster_name
      cluster_domain           = var.cluster_domain
      apl_values_repo_url      = var.apl_values_repo_url
      apl_values_repo_username = var.apl_values_repo_username
      apl_values_repo_password = var.apl_values_repo_token
      apl_values_repo_ref      = var.apl_values_repo_revision
      linode_dns_token         = var.linode_dns_token
      loki_admin_password      = var.loki_admin_password
      coredns_cluster_ip       = try(data.kubernetes_service.coredns[0].spec[0].cluster_ip, "")
      loki_bucket_chunks       = local.loki_buckets.chunks
      loki_bucket_ruler        = local.loki_buckets.ruler
      loki_bucket_admin        = local.loki_buckets.admin
      loki_s3_endpoint         = local.s3_host
      loki_s3_region           = local.s3_region
      harbor_bucket            = local.harbor_bucket
      harbor_s3_endpoint       = local.obj_s3_url
      harbor_s3_region         = local.s3_region
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

resource "local_file" "apl_rendered_values" {
  filename        = "${path.module}/generated/${var.apl_values_env}-rendered-values.yaml"
  content         = local.apl_rendered_values
  file_permission = "0600"

  lifecycle {
    precondition {
      condition     = local.env_revision_in_configmap == var.apps_repo_revision
      error_message = <<-EOM
        env-revision-configmap.yaml mismatch detected.

          apl-values/${var.apl_values_env}/manifest/env-revision-configmap.yaml
          data.revision = "${local.env_revision_in_configmap}"

          var.apps_repo_revision = "${var.apps_repo_revision}"
          (set via TF_VAR_apps_repo_revision; defaults to "main")

        These MUST match. The configmap drives the targetRevision of every
        in-repo child Argo CD Application; the var drives the bootstrap
        App's own revision. A mismatch means the bootstrap App syncs from
        one branch while child Apps sync from another — typically stale
        content from `main`.

        Fix:
          1. Either set APPS_REPO_REVISION on infra-${var.apl_values_env}
             to "${local.env_revision_in_configmap}", OR
          2. Edit apl-values/${var.apl_values_env}/manifest/env-revision-configmap.yaml
             data.revision to "${var.apps_repo_revision}" and commit
             on the same branch the bootstrap is pointing at.
      EOM
    }
  }
}

# Idempotent namespace creation — same pattern as `argocd_namespace` below.
# Two things this resource handles together:
#
#  1. Helm's --create-namespace flag creates the ns OUTSIDE the release, so
#     cleanup_on_fail doesn't reverse it. On a failed-then-retried apply,
#     Helm refuses to re-create the existing namespace and the whole
#     helm_release errors out. Owning the namespace via server-side apply
#     makes the retry path idempotent.
#
#  2. Apl-core's chart ships templates/00-namespace.yaml — i.e. the chart
#     itself wants to manage the namespace. Without ownership annotations
#     Helm refuses to adopt the pre-existing namespace ("cannot be imported
#     into the current release"). We pre-tag the namespace with the three
#     Helm ownership markers (managed-by label + release-name and
#     release-namespace annotations) so the chart's render finds a namespace
#     already labeled as belonging to it, and the install proceeds without
#     the import collision.
#
# Trade-off: pre-adopting via these annotations means Helm WILL delete the
# namespace on `helm uninstall`. That's the same behavior as the original
# create_namespace=true path; the difference is that this resource will
# re-create the namespace idempotently on the next apply.
resource "kubectl_manifest" "apl_operator_namespace" {
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Namespace"
    metadata = {
      name = "apl-operator"
      labels = {
        "lke-landing-zone.akamai.io/managed-by-bootstrap" = "true"
        "app.kubernetes.io/managed-by"                    = "Helm"
      }
      annotations = {
        "meta.helm.sh/release-name"      = "apl"
        "meta.helm.sh/release-namespace" = "apl-operator"
      }
    }
  })
  server_side_apply = true
  force_conflicts   = true
}

# ── apl-sops-secrets — empty placeholder ─────────────────────────────────────
# apl-core 5.0.0's apl-operator chart unconditionally references
# `apl-sops-secrets` Secret via envFrom in templates/deployment.yaml:49-51,
# but `templates/secrets.yaml` only CREATES that Secret when `kms.sops` is
# set in chart values (it isn't — we don't use SOPS). The mismatch leaves
# apl-operator pods stuck in CreateContainerConfigError on every rollout.
#
# Observed during bootstrap: the first apl-operator pod
# (created by Helm install with no envFrom) ran fine. Argo CD's
# `apl-operator-apl-operator` Application then reconciled the Deployment
# from apl-core source, which DOES set envFrom, and the new ReplicaSet
# stalled because `apl-sops-secrets` doesn't exist. The old pod kept service
# alive but the cluster was in a permanent half-rollout state — and on a
# fresh bootstrap there's no old pod to fall back to.
#
# Workaround: TF creates an EMPTY `apl-sops-secrets` Secret so the
# Deployment's envFrom resolves (kubelet treats envFrom of an empty Secret
# as a no-op rather than CreateContainerConfigError). The apl-operator
# binary calls `getK8sSecret('apl-sops-secrets')` at runtime
# (src/operator/installer.ts:149) and only USES the SOPS_AGE_KEY value if
# SOPS-encrypted secrets are configured in apl-values; we're not, so the
# empty Secret is sufficient.
#
# If/when apl-core upstream fixes the chart (conditional envFrom on
# `hasKey $kms "sops"`), this resource can be deleted.
resource "kubectl_manifest" "apl_sops_secrets_placeholder" {
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name      = "apl-sops-secrets"
      namespace = "apl-operator"
      labels = {
        "lke-landing-zone.akamai.io/managed-by-bootstrap" = "true"
      }
      annotations = {
        "lke-landing-zone.akamai.io/note" = "Empty placeholder; apl-operator may populate at runtime if SOPS is configured"
      }
    }
    type = "Opaque"
    # Empty data — kubelet accepts envFrom of an empty Secret. The operator
    # binary defensively handles missing keys (treats SOPS_AGE_KEY as
    # optional), so no key here means no SOPS decryption attempted — which
    # matches our configuration (no `kms.sops` in apl-values/<env>/values.yaml).
    data = {}
  })
  server_side_apply = true
  # Don't fight apl-operator's own writes to this Secret. apl-core's
  # bootstrap.ts (src/cmd/bootstrap.ts:95) may add SOPS_AGE_KEY at runtime
  # if SOPS gets configured later; that's fine, TF stays out of its way.
  lifecycle {
    ignore_changes = [yaml_body]
  }
  depends_on = [
    kubectl_manifest.apl_operator_namespace,
  ]
}

# ── block-storage StorageClass — must exist before apl-operator's helmfile ─────
# apl-operator (installed by helm_release.apl below) drives a helmfile pipeline
# that deploys ~20+ component charts, many of which create PVCs (gitea, harbor,
# keycloak DB Clusters, prometheus, etc.). Those charts read
# `cluster.defaultStorageClass: block-storage-retain` from
# apl-values/<env>/values.yaml and set their PVC spec.storageClassName
# accordingly — so the class MUST exist in the apiserver before helmfile starts
# the first PVC-creating chart.
#
# Landing the SC via TF (instead of Argo CD's platform-bootstrap App at wave -20
# in the old layout) closes the race window: TF applies the SC, then
# helm_release.apl returns, then apl-operator picks up the helmfile.
#
# The manifest itself documents the parameter-key trivia (encrypted/volumeTags,
# not encryption/volume-tags). The Linode CSI driver silently ignored the old
# spelling, leaving every "encrypted" Volume actually unencrypted until this PR.
resource "kubectl_manifest" "platform_app_storage_class" {
  yaml_body         = file("${path.module}/manifests/block-storage-class.yaml")
  server_side_apply = true
  field_manager     = "cluster-bootstrap-tf"
}

resource "helm_release" "apl" {
  name             = "apl"
  repository       = "https://linode.github.io/apl-core"
  chart            = "apl"
  version          = var.apl_chart_version
  namespace        = "apl-operator"
  create_namespace = false

  values = [local.apl_rendered_values]

  depends_on = [
    kubectl_manifest.apl_operator_namespace,
    kubectl_manifest.platform_app_storage_class,
  ]

  # Apl-core's helmfile pipeline runs ~40 component installs in sequence; on a
  # fresh cluster the total bootstrap takes 10-15 minutes per techdocs. The
  # chart's helm-release-level wait covers only the apl-operator Deployment;
  # downstream component readiness is observed via Argo CD afterwards.
  #
  # timeout=600 (not 900): `wait` only gates the operator Deployment becoming
  # Available (a couple minutes even with a cold image pull), so install needs
  # far less than this. This value used to also bound a DESTROY-time hang — the
  # helm provider waits up to `timeout` for the release's resources to delete,
  # and that uninstall routinely sat at "Still destroying..." on finalizer-heavy
  # state (CNPG clusters, Argo apps, namespaces) and then failed with "context
  # deadline exceeded". That hang is now avoided entirely: the DESTROY Cluster
  # Bootstrap job drops this release (and its apl-* scaffolding) from Terraform
  # state before `terraform destroy` (the "Untrack cluster-bootstrap resources"
  # step → `llz ci tf-untrack`), so
  # no in-cluster uninstall is attempted on teardown. The DESTROY Cluster job
  # then deletes the LKE cluster, which reaps every in-cluster resource (incl.
  # this release) regardless. The timeout therefore only matters on install now.
  # Manual fallback if you ever destroy outside CI: `terraform state rm
  # helm_release.apl` (+ the apl_* kubectl_manifests) and let the cluster delete
  # reap them.
  timeout       = 600
  wait          = true
  wait_for_jobs = true

  cleanup_on_fail = true

  # Upgrade only when apl_chart_version is bumped in tfvars.
  lifecycle {
    ignore_changes = [version]
  }
}

# ── Values-repo Git credentials (HTTPS PAT) ──────────────────────────────────
# Argo CD reads this Secret to authenticate against the values repo. apl-core
# normally creates this from `otomi.git.*` at install time; we seed it via TF
# as well so the in-cluster Argo can pull immediately even before the
# apl-operator's reconcile loop has applied the otomi-derived Secret.
#
# Server-side apply via kubectl_manifest is idempotent; if apl-core later
# overwrites this Secret with its own version, the credential payload is the
# same (Terraform variable → apl-core values → in-cluster Secret), so no fight.
#
# N4-M1 — apl-core MAY create its own values-repo Secret under a different
# name (otomi-values-repo, argocd-otomi-values-repo, etc.). Verify in lab
# with `kubectl -n argocd get secret -l argocd.argoproj.io/secret-type=repository`.
# If a second Secret points at the same URL it's harmless (Argo CD picks
# whichever matches); delete this TF-managed one if you prefer apl-core to
# be sole owner.
resource "kubectl_manifest" "apl_values_repo_creds" {
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name      = "apl-values-repo"
      namespace = "argocd"
      labels = {
        "argocd.argoproj.io/secret-type" = "repository"
      }
    }
    type = "Opaque"
    stringData = {
      type     = "git"
      url      = var.apl_values_repo_url
      username = var.apl_values_repo_username
      password = var.apl_values_repo_token
    }
  })
  server_side_apply = true
  force_conflicts   = true
  depends_on = [
    kubectl_manifest.argocd_namespace,
  ]
}

# ── Argo CD namespace ────────────────────────────────────────────────────────
# helm_release.apl wait=true only blocks until the apl-operator Deployment is
# Ready; the apl-operator then runs the helmfile in-cluster, which can take
# 10-15 more minutes before the argocd namespace exists.
#
# Create the namespace ourselves via kubectl_manifest with server-side apply
# (idempotent on already-exists, unlike kubernetes_namespace_v1). apl-core
# will adopt it later when its helmfile runs — additive labels/annotations
# from apl-core won't cause TF to fight back because server-side apply with
# force_conflicts merges field ownership.
resource "kubectl_manifest" "argocd_namespace" {
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Namespace"
    metadata = {
      name = "argocd"
      labels = {
        # apl-core's auto-generated NetworkPolicies (notably
        # gitea/gitea-platform-policy from the apl-network-policies chart)
        # use namespaceSelector `matchLabels: { name: argocd }` to authorize
        # argocd-repo-server ingress to gitea-http. Without this label the
        # selector doesn't match → `gitops-global` Argo Application times
        # out cloning the in-cluster gitea repo with `context deadline
        # exceeded`. EVERY OTHER apl-core-managed namespace (istio-system,
        # monitoring, apl-operator, apl-gitea-operator, otomi, cnpg-system,
        # harbor, cert-manager, grafana, keycloak) ships with the `name=<ns>`
        # label from apl-core's helmfile; argocd's namespace is set here in
        # TF so it was missing the convention. Observed: applying the label
        # flipped `gitops-global` from
        # Unknown/Healthy (timed out) to Synced/Healthy within 30s.
        name                                              = "argocd"
        "lke-landing-zone.akamai.io/managed-by-bootstrap" = "true"
      }
    }
  })
  server_side_apply = true
  force_conflicts   = true
  depends_on        = [helm_release.apl]
}

# ── Bootstrap bridge to the instance-repo manifest tree ──────────────────────
# apl-operator does NOT pass server.additionalApplications through its
# _rawValues filter (empirical test on primary, commit af31e76 — the values
# land but the Application never materializes), so the kustomize tree at
# apl-values/<env>/manifest/ would never sync and the cluster would sit without
# ESO CRDs, foundation NPs, AppProjects, etc.
#
# Fallback: declare the bridge directly via TF. Two kubectl_manifest
# resources (repo Secret + bootstrap Application) plus a CRD-wait so future
# fresh-cluster applies don't race apl-operator's helmfile that installs
# argo-cd. Both point at the instance repo over HTTPS with the values-repo PAT.

# Real readiness gate for the apl-core pipeline.
#
# CONTEXT — see docs/architecture/convergence-contract.md:
#   helm_release.apl's built-in wait only covers the apl-operator Deployment.
#   apl-operator then runs a helmfile pipeline that installs ~40 components
#   (Argo CD, Kyverno, cert-manager, ESO, observability stack, harbor, etc.)
#   sequentially over 10-15 minutes. Everything TF does after helm_release.apl
#   returns races that pipeline.
#
# `llz ci wait-apl-pipeline` (tools/cmd/llz/ci_wait_apl_pipeline.go) blocks until
# the three platform prerequisites are really SERVING — Argo CD application-
# controller (readyReplicas), the Kyverno admission controller (Available), and
# the cert-manager webhook (Available) — each gated on real readiness, not mere
# CRD-Established (a CRD is Established ~60-90s before its controller serves). It
# FAILS LOUD on a timeout (the convergence contract rejects soft-fail-and-
# continue, which is how bootstraps declare success while half-broken) and dumps
# apl-operator pods + logs when a resource never appears. The per-stage rationale,
# the StatefulSet-has-no-Ready-condition quirk, and the deliberately-omitted ESO
# and gitea stages are all documented in that command.
#
# This REPLACES the prior wait_for_argo_application_crd + wait_for_kyverno_crd
# null_resources AND the ~100-line inline bash that lived here: the poll/wait
# state machine is now unit-tested Go driven through injected kubectl/clock seams.
#
# Once it returns, every downstream TF resource that depends on this gate
# (platform-bootstrap Application + AppProject, the Kyverno race policies) can
# apply without racing the pipeline.
resource "null_resource" "apl_pipeline_ready" {
  triggers = {
    apl_release = helm_release.apl.id
  }
  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = "llz ci wait-apl-pipeline"
    environment = {
      KUBECONFIG_RAW = local.kubeconfig_raw
    }
  }
  depends_on = [helm_release.apl]
}

# Apply the PVC-encryption ClusterPolicy via a null_resource + kubectl
# (instead of kubectl_manifest) so we can wait for Kyverno then apply.
# kubectl_manifest validates the CRD-instance at plan time and would
# hard-fail; the local-exec wrapper polls for Kyverno's readiness first.
#
# The policy YAML is kept in the cluster-bootstrap module (not under
# apl-values/) so it lands via TF, NOT through the apl-bootstrap Argo
# Application — which would synchronize AFTER apl-core's helmfile has
# already created harbor/gitea/keycloak PVCs.
#
# TIMING IS THE WHOLE POINT. The policy is admission-only (background: false)
# and PVC storageClassName is immutable, so it MUST be enforcing BEFORE
# apl-operator's helmfile creates the gitea-valkey / oauth2-proxy PVCs — those
# two charts ignore cluster.defaultStorageClass and hardcode
# linode-block-storage, so this mutation is their only path onto the
# encrypted + block-storage-tagged SC.
#
# It deliberately does NOT depend on null_resource.apl_pipeline_ready: that
# gate also waits for argocd + cert-manager, and that extra ~minute is exactly
# the window in which apl-operator races ahead and provisions the PVCs
# unmutated (observed: the policy reached Ready ~30-60s AFTER
# gitea-valkey's PVC was created, so it slipped through permanently). Instead
# this depends only on helm_release.apl (apl-operator installed) and polls for
# Kyverno's admission controller itself, applying the instant Kyverno can admit
# the policy — well before the helmfile reaches the workload charts.
#
# NOTE — this narrows the race to near-zero but is not a hard guarantee: apl-
# operator and this provisioner are independent. The only race-free option is
# to ship the ClusterPolicy inside apl-core's own helmfile ordering (right
# after the kyverno release), which needs apl-core custom-policy support;
# tracked as a follow-up.
#
# All four kyverno_* policy resources share the `llz ci apply-kyverno-policy`
# command (tools/cmd/llz/ci_kyverno.go, baked into the ci-terraform image):
# kubeconfig tempfile + cleanup, an optional Kyverno-admission readiness poll
# (WAIT_FOR_KYVERNO, 15m deadline → warn + exit 0), a server-side kubectl apply,
# and a soft-fail (warn + exit 0) when the apply hits the transient kyverno-svc
# admission-webhook race. Per-policy behavior — manifest, whether to poll, and
# the exact ::warning:: texts — is injected via the environment block;
# triggers/depends_on stay per-resource. (The apply logic now lives in the
# pinned llz binary rather than a module script, so there is no script_sha
# trigger; policies re-apply on apl_release or manifest-content changes.)
resource "null_resource" "kyverno_pvc_encrypted_policy" {
  triggers = {
    apl_release     = helm_release.apl.id
    policy_yaml_sha = filesha256("${path.module}/manifests/kyverno-pvc-encrypted-storage-class.yaml")
  }
  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = "llz ci apply-kyverno-policy"
    environment = {
      KUBECONFIG_RAW  = local.kubeconfig_raw
      POLICY_MANIFEST = "${path.module}/manifests/kyverno-pvc-encrypted-storage-class.yaml"
      # Poll then apply IMMEDIATELY — do NOT wait on the rest of
      # apl_pipeline_ready (argocd/cert-manager); every second here is a second
      # in which apl-operator's helmfile may create the gitea-valkey /
      # oauth2-proxy PVCs before this mutation enforces.
      WAIT_FOR_KYVERNO     = "true"
      TIMEOUT_WARNING      = "Kyverno admission controller not Ready within 15m — skipping PVC policy apply. gitea-valkey + oauth2-proxy redis PVCs may land on linode-block-storage; re-run terraform apply once Kyverno is up."
      WEBHOOK_RACE_WARNING = "Kyverno admission webhook not yet reachable — policy apply skipped. Re-run terraform apply once kyverno-svc has Ready endpoints."
    }
  }
  depends_on = [
    helm_release.apl,
  ]
}

# Apply the Loki-S3-object_store ClusterPolicy. Same null_resource + local-exec
# pattern as kyverno_pvc_encrypted_policy above (CRD-existence guard + soft-fail
# on the kyverno-svc webhook race), applied at kyverno-ready so the policy is in
# place BEFORE apl-core's loki helmfile phase creates the loki ConfigMap — the
# mutation rewrites object_store filesystem->s3 on the cm's CREATE admission, so
# Loki reads the s3 schema from first start and persists chunks to S3 instead of
# the read-only container FS. See manifests/kyverno-loki-s3-object-store.yaml for
# why this can't be a values override (apl-core's pipeline corrupts the schema
# date).
resource "null_resource" "kyverno_loki_s3_object_store" {
  triggers = {
    apl_release     = helm_release.apl.id
    policy_yaml_sha = filesha256("${path.module}/manifests/kyverno-loki-s3-object-store.yaml")
  }
  # Shared apply wrapper — see kyverno_pvc_encrypted_policy above. Polls for
  # kyverno-ready (don't gate on the rest of apl_pipeline_ready; loki installs
  # in a later apl-core helmfile phase, so landing this at kyverno-ready leaves
  # ample margin before the loki cm).
  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = "llz ci apply-kyverno-policy"
    environment = {
      KUBECONFIG_RAW       = local.kubeconfig_raw
      POLICY_MANIFEST      = "${path.module}/manifests/kyverno-loki-s3-object-store.yaml"
      WAIT_FOR_KYVERNO     = "true"
      TIMEOUT_WARNING      = "Kyverno admission controller not Ready within 15m — skipping loki-s3 policy apply. Loki will write to the read-only FS and persist no logs; re-run terraform apply once Kyverno is up."
      WEBHOOK_RACE_WARNING = "Kyverno admission webhook not yet reachable — loki-s3 policy apply skipped. Re-run terraform apply once kyverno-svc has Ready endpoints."
    }
  }
  depends_on = [
    helm_release.apl,
  ]
}

# The loki-gateway nginx-resolver Kyverno policy was RETIRED here. The grafana/
# loki gateway templates nginx `resolver <dnsService>...;` as a hostname, which
# nginx can't use — it crashloops. That is now fixed at the chart source: the
# coredns ClusterIP is read at bootstrap (data.kubernetes_service.coredns) and
# rendered into apps.loki._rawValues.gateway.nginxConfig.resolver, so the gateway
# ships a usable resolver from first sync. That replaces the Kyverno admission
# mutation, which depended on the webhook being up when the cm was created and
# crashlooped the gateway permanently when Kyverno lagged (the failure mode this
# null_resource + manifests/kyverno-loki-gateway-resolver.yaml existed to patch).
# Validated by a full e2e off main. (`llz ci apply-kyverno-policy`'s RETROFIT_*
# capability is retained for reuse but no longer driven by any policy here.)

# Apply the oauth2-proxy wait-for-keycloak TLS-trust ClusterPolicy. Same
# null_resource + local-exec pattern as kyverno_pvc_encrypted_policy above —
# CRD existence guard + soft-fail on the kyverno-svc webhook race. Kept as a
# separate resource (rather than folded into the PVC one) so each policy's
# trigger/apply lifecycle is independently inspectable in TF state.
resource "null_resource" "kyverno_oauth2_proxy_wait_keycloak_trust" {
  triggers = {
    apl_release     = helm_release.apl.id
    policy_yaml_sha = filesha256("${path.module}/manifests/kyverno-oauth2-proxy-wait-keycloak-trust.yaml")
  }
  # Shared apply wrapper — see kyverno_pvc_encrypted_policy above. No readiness
  # poll (WAIT_FOR_KYVERNO=false): this resource already depends on
  # apl_pipeline_ready, so only the CRD-existence guard applies here.
  provisioner "local-exec" {
    interpreter = ["bash", "-c"]
    command     = "llz ci apply-kyverno-policy"
    environment = {
      KUBECONFIG_RAW       = local.kubeconfig_raw
      POLICY_MANIFEST      = "${path.module}/manifests/kyverno-oauth2-proxy-wait-keycloak-trust.yaml"
      WAIT_FOR_KYVERNO     = "false"
      CRD_MISSING_WARNING  = "Kyverno ClusterPolicy CRD not present — skipping oauth2-proxy wait-for-keycloak mutation. oauth2-proxy init container will loop until the policy lands."
      WEBHOOK_RACE_WARNING = "Kyverno admission webhook not yet reachable — oauth2-proxy mutation apply skipped. Re-run terraform apply once kyverno-svc has Ready endpoints."
    }
  }
  depends_on = [
    null_resource.apl_pipeline_ready,
  ]
}

# Repo Secret — ArgoCD reads this to authenticate against the platform-apps
# repo (the instance repo) over HTTPS. Labeled
# argocd.argoproj.io/secret-type=repository so ArgoCD's repo-server discovers
# it automatically. Same instance repo and same fine-grained PAT as the
# apl-values-repo Secret above (apl-core's otomi.git points at the same repo).
# HTTPS basic-auth with a PAT needs no SSH host-key handling, so the former
# argocd-ssh-known-hosts-cm ConfigMap and the ssh-keyscan data source are gone.
resource "kubectl_manifest" "argocd_apps_repo" {
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name      = "platform-apps-repo"
      namespace = "argocd"
      labels = {
        "argocd.argoproj.io/secret-type" = "repository"
      }
    }
    type = "Opaque"
    stringData = {
      type     = "git"
      url      = "https://github.com/<@ instance_repo @>.git"
      username = var.apl_values_repo_username
      password = var.apl_values_repo_token
    }
  })
  server_side_apply = true
  force_conflicts   = true
  depends_on        = [kubectl_manifest.argocd_namespace]
}

# Repo Secret — lets ArgoCD authenticate to GHCR to pull the first-party OCI
# Helm charts (ghcr.io/<@ upstream_org @>/charts/*: cluster-foundation, openbao-
# platform, cert-automation, internal-cidr-firewall). These
# packages are PUBLIC, so ArgoCD pulls them anonymously and this Secret is
# normally NOT created — it exists only for a private fork that keeps its charts
# private. type=helm + enableOCI=true; url is the registry+org prefix ArgoCD
# matches the Application's repoURL against. Gated on ghcr_token: empty (the
# default, public-charts path) skips it; plan/destroy work without it.
resource "kubectl_manifest" "argocd_ghcr_oci_creds" {
  count = var.ghcr_token != "" ? 1 : 0
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name      = "ghcr-charts-oci"
      namespace = "argocd"
      labels = {
        "argocd.argoproj.io/secret-type" = "repository"
      }
    }
    type = "Opaque"
    stringData = {
      type      = "helm"
      url       = "ghcr.io/<@ upstream_org @>/charts"
      enableOCI = "true"
      username  = var.ghcr_username
      password  = var.ghcr_token
    }
  })
  server_side_apply = true
  force_conflicts   = true
  depends_on        = [kubectl_manifest.argocd_namespace]
}

# Image-pull Secret for private ghcr.io images using the SAME GHCR read token
# ArgoCD uses for the OCI charts above. The Harbor robot pull secret cannot
# authenticate to ghcr.io, so a chart's imagePullSecrets points here. Used by the
# optional Akamai-internal llz-linode-cidr-firewall controller, whose private image
# is ghcr.io/<@ upstream_org @>/firewall-controller-internal (added back per
# docs/consume-lke-landing-zone-internal.md). Gated on ghcr_token like the repo
# Secret — without it the image must be public.
resource "kubectl_manifest" "ghcr_image_pull_secret" {
  count = var.ghcr_token != "" ? 1 : 0
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name      = "ghcr-pull-secret"
      namespace = "kube-system"
    }
    type = "kubernetes.io/dockerconfigjson"
    stringData = {
      ".dockerconfigjson" = jsonencode({
        auths = {
          "ghcr.io" = {
            username = var.ghcr_username
            password = var.ghcr_token
            auth     = base64encode("${var.ghcr_username}:${var.ghcr_token}")
          }
        }
      })
    }
  })
  server_side_apply = true
  force_conflicts   = true
}

# AppProject for the bootstrap Application — restricts what the seed app
# can pull and where it can deploy. sourceRepos is pinned to the bits URL
# so a compromised Application spec elsewhere can't redirect this seed at
# a different repo. destinations/whitelists are intentionally permissive
# because the kustomize tree under apl-values/<env>/manifest/ creates
# resources across many namespaces (argocd, openbao, harbor,
# observability, cert-manager, etc.) and many kinds (NetworkPolicies,
# StorageClasses, AppProjects, child Applications, ConfigMaps, Secrets,
# CRDs). The scoping value here is the source pin; once individual
# downstream apps are reconciled they live under their own per-domain
# AppProjects (platform-support) defined inside the manifest tree
# itself, which can be more aggressively restricted.
resource "kubectl_manifest" "app_bootstrap_appproject" {
  yaml_body = yamlencode({
    apiVersion = "argoproj.io/v1alpha1"
    kind       = "AppProject"
    metadata = {
      name      = "platform-bootstrap"
      namespace = "argocd"
    }
    spec = {
      description = "Seed project for the apl-values manifest tree app-of-apps. Source-pinned to the instance repo over HTTPS."
      sourceRepos = ["https://github.com/<@ instance_repo @>.git"]
      destinations = [
        {
          server    = "https://kubernetes.default.svc"
          namespace = "*"
        }
      ]
      clusterResourceWhitelist = [
        {
          group = "*"
          kind  = "*"
        }
      ]
      namespaceResourceWhitelist = [
        {
          group = "*"
          kind  = "*"
        }
      ]
    }
  })
  server_side_apply = true
  # No force_conflicts: nothing in apl-core's chart creates an AppProject
  # matching this name. The flag was defensive paranoia from the same era
  # as app_bootstrap_application's (now also removed); auditing the
  # ownership surface in R10 confirmed there's no competing writer.
  depends_on = [
    null_resource.apl_pipeline_ready,
  ]
}

# Bootstrap Application — points ArgoCD at apl-values/<env>/manifest/ on the
# instance repo (HTTPS). The kustomize tree there pulls in _shared/manifest/ which
# contains AppProjects, foundation NetworkPolicies + StorageClass, ESO
# install, Argo Workflows + Events (cert-automation), OpenBao,
# firewall-controller, cert-manager extras, and observability dashboards. Sync policy: automated
# with prune + selfHeal so the cluster stays in lockstep with git after
# bootstrap. Scoped to the platform-bootstrap AppProject above so the source
# repo is pinned at the project layer.
resource "kubectl_manifest" "app_bootstrap_application" {
  yaml_body = yamlencode({
    apiVersion = "argoproj.io/v1alpha1"
    kind       = "Application"
    metadata = {
      name      = "platform-bootstrap"
      namespace = "argocd"
    }
    spec = {
      project = "platform-bootstrap"
      source = {
        repoURL        = "https://github.com/<@ instance_repo @>.git"
        targetRevision = var.apps_repo_revision
        path           = "apl-values/${var.apl_values_env}/manifest"
      }
      destination = {
        server    = "https://kubernetes.default.svc"
        namespace = "argocd"
      }
      syncPolicy = {
        automated = {
          prune    = true
          selfHeal = true
        }
        # Retry is load-bearing on first boot. Several wave-5 consumers depend on
        # an async, partly out-of-band chain — OpenBao deployed -> auto-unsealed ->
        # `eso-approle-secret` seeded by bootstrap-openbao.yml -> the `openbao`
        # ClusterSecretStore goes Ready -> its ExternalSecrets sync. If the sync
        # races ahead of that, the store's health-gate fails the wave; with NO
        # retry block a single failed automated sync becomes terminal and Argo
        # will NOT re-attempt the same revision (selfHeal only corrects drift
        # after a *successful* sync), wedging the cluster until a manual
        # `argocd app sync` or a new commit. ~20 retries @ 3m cap (~1h) rides out
        # the first-boot convergence window. Pairs with the lenient
        # ClusterSecretStore/ExternalSecret health customizations in
        # apl-values/_shared/values.yaml (apps.argocd._rawValues.configs.cm) so a
        # not-yet-ready store reports Progressing (wait), not Degraded (fail).
        retry = {
          limit = 20
          backoff = {
            duration    = "15s"
            factor      = 2
            maxDuration = "5m"
          }
        }
        # SkipDryRunOnMissingResource=true is critical here. The kustomize
        # tree includes both CRD-installing Applications (ESO, argo-workflows,
        # argo-events at wave -15) AND resources whose CRDs those installs
        # provide (ExternalSecret, Sensor, EventBus, WorkflowTemplate at
        # default wave 0). Without this option ArgoCD pre-sync dry-runs the
        # entire rendered manifest in one pass; the dry-run fails on the
        # consumers' missing CRDs, the whole sync aborts, and the install
        # Applications never get a chance to register the CRDs. With it,
        # missing-CRD resources fail dry-run silently, the install Apps land,
        # CRDs register, and on the next reconcile the consumers succeed.
        # See: https://argo-cd.readthedocs.io/en/stable/user-guide/sync-options/#skip-dry-run-for-new-custom-resources-types
        syncOptions = [
          "CreateNamespace=true",
          "ServerSideApply=true",
          "SkipDryRunOnMissingResource=true",
        ]
      }
    }
  })
  server_side_apply = true
  # force_conflicts was previously needed because every env's values.yaml
  # also rendered an `platform-bootstrap` Application via apl-core's chart
  # (apps.argocd._rawValues.server.additionalApplications). That duplicate
  # was deleted in the convergence-contract cleanup so TF is sole owner
  # now; no conflict to force.
  depends_on = [
    null_resource.apl_pipeline_ready,
    kubectl_manifest.argocd_apps_repo,
    kubectl_manifest.app_bootstrap_appproject,
  ]
}

# ── llz-secret-store Application (blast-radius isolation for the ClusterSecretStore) ──
# The `openbao` ClusterSecretStore is the single binding EVERY ExternalSecret
# depends on, but on first boot it cannot go Ready until OpenBao is unsealed and
# bootstrap-openbao.yml has seeded `eso-approle-secret`. When it lived in the
# platform-bootstrap kustomize tree, its not-ready health FAILED that app's whole
# sync wave (the exact hook that wedged the first lab bootstrap), stranding
# unrelated wave-5 resources. Carving it into its own Application lets it converge
# on its own retry loop without gating anything else — and nothing else gates it.
#
# Source is a fixed, env-agnostic path (the store references only fixed names), so
# it does NOT go through the per-env `llz render` overlay; it is pinned to the same
# apps_repo_revision as platform-bootstrap. SkipDryRunOnMissingResource rides out
# the window before platform-bootstrap installs the external-secrets CRDs; prune is
# off because the store is load-bearing. Same project as platform-bootstrap (it
# already permits the cluster-scoped ClusterSecretStore + this source repo).
resource "kubectl_manifest" "app_secret_store_application" {
  yaml_body = yamlencode({
    apiVersion = "argoproj.io/v1alpha1"
    kind       = "Application"
    metadata = {
      name      = "llz-secret-store"
      namespace = "argocd"
    }
    spec = {
      project = "platform-bootstrap"
      source = {
        repoURL        = "https://github.com/<@ instance_repo @>.git"
        targetRevision = var.apps_repo_revision
        path           = "apl-values/_shared/manifest-secret-store"
      }
      destination = {
        server    = "https://kubernetes.default.svc"
        namespace = "argocd"
      }
      syncPolicy = {
        automated = {
          prune    = false
          selfHeal = true
        }
        retry = {
          limit = 20
          backoff = {
            duration    = "15s"
            factor      = 2
            maxDuration = "5m"
          }
        }
        syncOptions = [
          "ServerSideApply=true",
          "SkipDryRunOnMissingResource=true",
        ]
      }
    }
  })
  server_side_apply = true
  depends_on = [
    null_resource.apl_pipeline_ready,
    kubectl_manifest.argocd_apps_repo,
    kubectl_manifest.app_bootstrap_appproject,
  ]
}

# NOTE — the prior `argocd_config_patcher_job` and its SA/Role/RoleBinding
# locals (≈130 lines) were removed in the convergence-contract anti-pattern
# cleanup. The argocd-cm config they patched (`kustomize.buildOptions` for
# `--load-restrictor LoadRestrictionsNone`, plus the Argo health.lua for
# ClusterSecretStore / ExternalSecret added in the same PR) is now set
# declaratively through `apl-core` values at
# `apl-values/<env>/values.yaml::apps.argocd._rawValues.configs.cm.*`.
# See docs/architecture/convergence-contract.md anti-pattern #1 (patcher
# Jobs) — TF runs a K8s Job to `kubectl patch` a config Argo/apl-core owns
# is a two-system tug-of-war via Kubernetes Job and breaks the "one owner
# per resource" rule the contract names.

# NOTE — the prior `sc_default_patcher_job` resource cluster (SA +
# ClusterRole + ClusterRoleBinding + kubectl_manifest Job + ~50-line
# local script) lived here. It demoted LKE's
# `linode-block-storage-retain` from default so
# `block-storage-retain` would be the cluster default.
# Removed in the convergence-contract cleanup PR; replaced by an Argo
# CD PostSync hook Job at
# `apl-values/_shared/manifest/foundation/sc-default-patcher-job.yaml`
# (anti-pattern #1 — patcher Jobs run from TF). Argo now owns it: if
# Flux's `workload` HelmRelease reconciles and reverts the demotion,
# Argo's selfHeal + the PostSync hook re-running on the next sync put
# us back to "one default, and it's ours". TF no longer needs to know.

# ── Destroy-time orphan-Volume cleanup (now a no-op — see below) ──────────────
# Reaping orphaned Linode Block Storage Volumes on destroy keeps a destroyed
# cluster's Volumes from counting against the account-wide service quota — the
# failure mode that stalled gitea PVCs on the next bootstrap and silently
# blocked Argo CD's install.
#
# That sweep MOVED to the DESTROY Cluster job
# (.github/workflows/terraform.yml → "Sweep orphaned Block Storage Volumes"),
# which runs AFTER the LKE nodes are deleted — the only point at which the
# pvc-* Volumes are unattached and therefore actually deletable. It reuses
# `llz ci reap-volumes` (region-scoped, unattached-only)
# and catches ALL pvc-* Volumes regardless of reclaim policy (Retain AND
# Delete). Sweeping HERE was structurally useless: this provisioner runs while
# the nodes are still alive, so every Volume is still attached
# (linode_id != null), the unattached-only filter deleted nothing, and the
# old body burned ~240s on every destroy (a 180s `kubectl delete pvc -A` that
# can never complete while StatefulSet pods still mount the PVCs, plus a 60s
# settle window).
#
# This is intentionally a no-op resource, NOT a removed one. Its destroy-time
# provisioner and triggers must stay byte-stable so TF never plans a
# replacement: a replacement — or removing the resource — risks firing a
# destroy-time provisioner during a normal `apply` against a LIVE cluster, and
# the body this once held (`kubectl delete pvc --all -A`) would wipe every PVC
# on a running cluster. Only the command text was changed, which produces no
# plan diff and runs only on a real future destroy. The triggers below are now
# vestigial (the no-op references none of them) but retained for that reason.
resource "null_resource" "cleanup_platform_volumes_on_destroy" {
  triggers = {
    # base64 so the multiline kubeconfig survives the trigger map round-trip
    # and re-decodes cleanly inside the bash heredoc.
    kubeconfig_b64 = base64encode(local.kubeconfig_raw)
    linode_token   = var.linode_token
    # Tag we filter on. Hardcoded to match the volume-tags parameter in
    # apl-values/_shared/manifest/foundation/storageclass.yaml — keep in sync
    # if that ever changes.
    volume_tag = "block-storage"
  }

  provisioner "local-exec" {
    when        = destroy
    interpreter = ["bash", "-c"]
    command     = <<-EOT
      set -uo pipefail

      # No-op: the authoritative orphan-Volume sweep now runs in the DESTROY
      # Cluster job (.github/workflows/terraform.yml → "Sweep orphaned Block
      # Storage Volumes"), AFTER the LKE nodes are deleted and the Volumes
      # detach — the only window in which the unattached-only filter can match
      # anything. See the resource header comment for why this stays a no-op
      # instead of being removed (replacement/removal could fire a destroy
      # provisioner during a live `apply`).
      echo "orphan-Volume cleanup moved to the DESTROY Cluster job's post-node-delete sweep (llz ci reap-volumes) — nothing to do here."
    EOT
  }

  # depends_on ensures this resource is created after the cluster is
  # reachable and after apl-core has been installed; on destroy TF reverses
  # the order, so this resource's destroy-time provisioner runs before
  # helm_release.apl is uninstalled — i.e. while the cluster is still up and
  # serving the kubeconfig.
  depends_on = [helm_release.apl]
}

# ── Unwedge namespace finalization on destroy ───────────────────────────────
# `helm_release.apl` has `wait = true`, so its uninstall blocks until the
# release's resources (incl. namespaces) actually delete. Two things observed
# make that wait run out the full `timeout` and then ERROR:
#
#   1. Argo CD deadlock. apl-core installs Argo CD, which creates ~60+
#      Applications + AppProjects, each carrying `resources-finalizer.
#      argocd.argoproj.io`. helm uninstall removes the argocd controller, so
#      nothing is left to process those finalizers — the CRs are immortal and
#      the `argocd` namespace never finalizes (NamespaceContentRemaining /
#      NamespaceFinalizersRemaining stay True forever).
#   2. Broken aggregated discovery. A stale APIService whose backing Service
#      is gone (e.g. the cert-manager-webhook-linode ACME solver APIService)
#      reports Available=False, which fails
#      discovery for that group on EVERY namespace
#      (NamespaceDeletionDiscoveryFailure=True) and stalls finalization
#      cluster-wide.
#   3. Operator-managed CRs left holding finalizers. helm removes an
#      operator's Deployment, but the CRs it managed still carry that
#      operator's finalizer — with nothing left to process it, the CR is
#      immortal and its namespace never finalizes
#      (NamespaceContentRemaining). Observed: CNPG (CloudNativePG) Postgres
#      `Cluster`/`Pooler` CRs (the Harbor/Keycloak/Gitea back ends) carry
#      cnpg.io finalizers; once the cnpg-system operator is gone the
#      cnpg-system / harbor / keycloak / gitea namespaces hang in
#      Terminating and helm's apl uninstall --wait sits there until the
#      600s timeout, then ERRORs.
#
# This destroy-time provisioner runs BEFORE helm uninstalls apl (it
# depends_on helm_release.apl, so TF tears it down first, while the cluster
# is still serving the kubeconfig) and proactively clears both wedges so the
# uninstall + namespace GC complete quickly instead of timing out. Everything
# here is best-effort and non-fatal: if the cluster is already unreachable, or
# a call fails, we continue — the subsequent DESTROY Cluster job deletes the
# LKE cluster and reaps all of this regardless. The value is a clean, fast
# `terraform destroy` instead of a 10-15m hang that ends in an error.
resource "null_resource" "unwedge_namespace_finalizers_on_destroy" {
  triggers = {
    # base64 so the multiline kubeconfig survives the trigger map round-trip
    # and re-decodes cleanly inside the bash heredoc (same as the volume
    # cleanup resource above).
    kubeconfig_b64 = base64encode(local.kubeconfig_raw)
  }

  provisioner "local-exec" {
    when        = destroy
    interpreter = ["bash", "-c"]

    # Pass the kubeconfig through the environment, NOT interpolated into the
    # command string. Terraform echoes the rendered `command` to the log (the
    # "(local-exec): Executing: [\"bash\" \"-c\" ...]" line) but NEVER the
    # `environment` block — so inlining ${self.triggers.kubeconfig_b64} into the
    # command, as this once did, printed the full lke-admin kubeconfig
    # (including a live bearer token) in plaintext on every destroy. Referencing
    # $KUBECONFIG_B64 keeps the secret out of the logged command.
    environment = {
      KUBECONFIG_B64 = self.triggers.kubeconfig_b64
    }

    # The 4-phase unwedge state machine (healthz guard → scale down Argo → strip
    # Argo finalizers + delete → delete stale aggregated APIServices → strip CNPG
    # finalizers) is now `llz ci destroy-unwedge` (tools/cmd/llz/ci_destroy_unwedge.go):
    # unit-tested Go driven through an injected kubectl seam, same best-effort/
    # non-fatal semantics and per-phase rationale documented there. It reads the
    # kubeconfig from $KUBECONFIG_B64 (above), never from argv.
    command = "llz ci destroy-unwedge"
  }

  # Same hook as cleanup_platform_volumes_on_destroy: depends_on
  # helm_release.apl so TF runs this destroy-time provisioner BEFORE the helm
  # uninstall, while the kubeconfig still works.
  depends_on = [helm_release.apl]
}

# ── Clear cluster-scoped GH env secrets on destroy ──────────────────────────
# After a cluster destroy, the GH env secrets bound to the previous OpenBao /
# Harbor instances are not just stale — they're actively harmful on the next
# bootstrap. Concretely observed: the previous
# run's "Revoke root token" if:always() step revoked OPENBAO_ROOT_TOKEN, the
# next run's "Configure OpenBao" loaded the now-invalid token, lookup-self
# returned 403, the regen step skipped on a stale gate (since fixed), and the
# bootstrap was stuck. Even after the regen-gate fix, the
# unseal keys and Harbor creds are still bound to a destroyed cluster — on
# next apply the Initialize step would generate new ones (overwriting the
# stale values) but the brief window between apply-time secret-rewrite and
# the next run leaves the env in a confusing state.
#
# Simpler model: tear down the secrets when their backing cluster is torn
# down. Each fresh apply re-seeds whatever's needed via Initialize +
# Configure flows in bootstrap-openbao.yml.
#
# Token requirement: `repo` + `secrets:write` scope on this repo. Same PAT
# used by bootstrap-openbao.yml's seed steps (OPENBAO_SECRETS_WRITE_TOKEN).
# If the var is empty (default), the provisioner is a no-op and the operator
# clears the secrets manually.
resource "null_resource" "clear_openbao_secrets_on_destroy" {
  triggers = {
    # Trigger KEY stays `region` (referenced as self.triggers.region below and
    # baked into existing state) — only the source variable was renamed to
    # `deployment`. The value is unchanged.
    region              = var.deployment
    secrets_write_token = var.openbao_secrets_write_token
    # AppRole GH-secret suffix this cluster owns: empty for active/standalone
    # (base name), "_STANDBY" for the standby peer — mirrors the role branch in
    # bootstrap-openbao.yml's secret-id seed so destroy clears the right name.
    # ha_role is declared once in cluster/<deployment>.tfvars and read here via
    # the cluster workspace's remote state. try() guards the empty-outputs case
    # (cluster workspace already destroyed) — a bare reference hard-fails even
    # `plan -destroy`; see the RESILIENCE note in providers.tf. On a live
    # cluster try() resolves to the real value, so the trigger stays byte-stable.
    approle_secret_suffix = try(data.terraform_remote_state.cluster.outputs.ha_role, "") == "standby" ? "_STANDBY" : ""
  }

  provisioner "local-exec" {
    when        = destroy
    interpreter = ["bash", "-c"]

    # secrets_write_token is a secrets:write PAT — pass it via the environment,
    # never interpolated into the command. Terraform echoes the rendered command
    # to the log but NOT the environment block, so inlining the token here would
    # print it in plaintext on every destroy (same leak class as the unwedge
    # kubeconfig). region is not sensitive and stays inline.
    environment = {
      GH_SECRETS_WRITE_TOKEN = self.triggers.secrets_write_token
    }

    command = <<-EOT
      set -uo pipefail

      if [ -z "$GH_SECRETS_WRITE_TOKEN" ]; then
        echo "::warning::openbao_secrets_write_token not set — skipping GH env-secret cleanup. After this destroy, manually clear: gh secret delete OPENBAO_ROOT_TOKEN OPENBAO_UNSEAL_KEY_1 OPENBAO_UNSEAL_KEY_2 OPENBAO_UNSEAL_KEY_3 OPENBAO_APPROLE_SECRET_ID${self.triggers.approle_secret_suffix} HARBOR_ROBOT_NAME HARBOR_PASSWORD --env infra-${self.triggers.region}"
        exit 0
      fi

      # github.com is the default host, so GH_TOKEN alone authenticates.
      export GH_TOKEN="$GH_SECRETS_WRITE_TOKEN"
      ENV_NAME="infra-${self.triggers.region}"

      # Cluster-scoped GH env secrets — all bound to the OpenBao/Harbor that
      # existed in THIS cluster. Anything not on this list is operator-shared
      # (LINODE_API_TOKEN, GITHUB_TOKEN, APL_VALUES_REPO_TOKEN, TF_STATE_*,
      # OPENBAO_SECRETS_WRITE_TOKEN itself, etc.) and must NOT be deleted.
      SECRETS=(
        OPENBAO_ROOT_TOKEN
        OPENBAO_UNSEAL_KEY_1
        OPENBAO_UNSEAL_KEY_2
        OPENBAO_UNSEAL_KEY_3
        OPENBAO_APPROLE_SECRET_ID${self.triggers.approle_secret_suffix}
        HARBOR_ROBOT_NAME
        HARBOR_PASSWORD
      )

      for SECRET in "$${SECRETS[@]}"; do
        # `gh secret delete` returns non-zero (HTTP 404) when the secret
        # doesn't exist — treat that as a no-op, NOT a destroy failure.
        # Any other error (network, auth, scope) we surface to the operator
        # but still proceed: the cluster IS being destroyed, blocking the
        # whole destroy on a secrets-API hiccup is disproportionate.
        if gh secret delete "$SECRET" --env "$ENV_NAME" 2>/dev/null; then
          echo "Deleted GH env secret $ENV_NAME / $SECRET"
        else
          echo "::warning::Could not delete $ENV_NAME / $SECRET (already absent, or token lacks scope). Verify via: gh secret list --env $ENV_NAME"
        fi
      done

      echo "OpenBao/Harbor GH env-secret cleanup complete for $ENV_NAME."
    EOT
  }

  # No `depends_on` — this resource has nothing to create. The destroy
  # provisioner is the entire payload, and it doesn't need any other
  # cluster-bootstrap resource to be present for `gh secret delete` to work
  # (GH API is independent of the cluster's existence).
}

# ── Linode Volume relabeler — fully Argo-owned (no TF→kube crossing) ─────────
# The LKE-managed Linode CSI controller stamps volumes with label
# `<--volume-label-prefix><PV-name>` and the prefix is empty on LKE-E, so the
# UI shows `pvc-<uuid>` everywhere. The CSI driver exposes no SC parameter to
# override this — the only first-class fix is a Linode support ticket asking
# for LINODE_VOLUME_LABEL_PREFIX to be set on the managed controller (capped
# at 12 chars, no per-PVC template) or an upstream PR adding a label-template
# parameter (longer timeline).
#
# In the meantime: a CronJob that walks PVs and PUTs human-readable labels via
# the Linode Volumes API. The ENTIRE tree — namespace, the linode-api-token
# Secret (synced from OpenBao by ESO), CronJob, ServiceAccount, RBAC, script
# ConfigMap, NetworkPolicy — lives in apl-values/.../components/volumeLabeler/
# and reconciles via apl-core's in-cluster Argo CD.
#
# TF no longer owns the namespace or the token Secret. The Secret used to be a
# static var.linode_token written straight into the cluster here; it now arrives
# via ESO from secret/linode/api-token (the canonical, daily-rotated path —
# bootstrap seeds it from LINODE_API_TOKEN, platform-ci policy grants the read),
# so the labeler reads a rotating credential through the standard secrets
# pipeline instead of a token that goes stale the moment rotation first runs.
# Per-env REGION_SHORT is driven by each env's manifest overlay's
# linode-volume-labeler-region-patch.yaml.
