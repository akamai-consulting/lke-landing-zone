# llz-cluster-bootstrap

The platform-bootstrap provisioning module: everything Terraform does on a freshly
provisioned LKE-E cluster to hand it over to apl-core (App Platform) and Argo CD.
Extracted from the `terraform-iac-bootstrap/cluster-bootstrap` root so an instance
carries only a thin root (kubeconfig + values render + this module call) instead of
~900 lines of glue — consumed remotely at a pinned `?ref=`, like `llz-object-storage`.

## What it does

- Installs apl-core via `helm_release.apl` with the instance's rendered values, and
  gates on `null_resource.apl_pipeline_ready` (`llz ci wait-apl-pipeline`) until Argo CD,
  Kyverno, and cert-manager are really serving.
- Bridges Argo CD to the instance repo's `apl-values/<env>/manifest/` tree: the
  platform-bootstrap + secret-store Applications and their AppProjects, GHCR OCI creds +
  image-pull Secret, and the operator/argocd namespaces.
- Applies the two race-sensitive Kyverno policies that must beat apl-operator's non-Argo
  PVC creation / LKE Flux's default StorageClass (`manifests/`).

The `path.module`-relative reads (`manifests/`, the `generated/` values dump) resolve
inside the module; the instance-specific inputs (rendered values, kubeconfig, env
revision) are passed in by the root.

## Inputs

| Variable | Purpose |
|---|---|
| `apl_rendered_values` (sensitive) | apl-core values.yaml with the runtime secrets + coredns IP filled by the root's `templatefile` |
| `kubeconfig_raw` (sensitive) | cluster kubeconfig (from the cluster workspace's remote state) — the destroy-time untrack check |
| `env_revision_in_configmap`, `apps_repo_revision` | the git-revision precondition (env-revision-configmap must equal apps_repo_revision) |
| `apl_chart_version`, `apl_values_env` | apl-core chart version + deployment/env name |
| `ghcr_username`, `ghcr_token` (sensitive) | GHCR OCI credentials for Argo CD chart pulls |

Providers `helm`, `kubernetes`, `kubectl` are configured in the root (from the cluster
kubeconfig) and passed via `providers = { … }`.
