# Your own Kubernetes resources — the operator escape hatch

**THIS DIRECTORY IS YOURS.** The template ships it once and never touches it again
(it's `owned` in `.template-manifest`, so `copier update` skips it, and `llz render`
never writes into it). Drop your Kubernetes manifests here and Argo CD applies them
— no Terraform, no edits to the LLZ-managed bootstrap tree.

## Layout

It follows App Platform's GitOps convention
(https://techdocs.akamai.com/app-platform/docs/gitops), so what you know from the
platform docs applies here:

```
custom/
  namespaces/<namespace>/    # namespaced resources → synced INTO <namespace>
  global/                    # cluster-scoped resources (CRDs, ClusterRoles, ...)
```

- **One Argo CD Application per `namespaces/<ns>/` directory.** The namespace is
  created automatically if it does not exist.
- **Subdirectories are organizational only** — everything under a namespace
  directory is recursed and applied into that namespace.
- **A `kustomization.yaml` is optional.** A plain directory of manifests is applied
  as-is; add one only when you want kustomize features (patches, generators). If
  present at a directory's root, Argo builds it with kustomize instead of recursing.

## What syncs it

The `instance-custom` **ApplicationSet** (generated per env into
`apl-values/<env>/manifest/instance-custom.yaml` by `llz render`). It runs at
**sync-wave 10** — after the platform support plane is healthy — so your resources
can rely on cert-manager, External Secrets + the `openbao` ClusterSecretStore,
namespaces, and the default-deny NetworkPolicies already being up.

## Rules worth knowing

- **`apl-` is reserved.** Never create `namespaces/apl-*/`. Those namespaces belong
  to apl-core, whose own `gitops-ns-apl-*` Applications already manage them; a
  second Application over the same resources puts them in contention. `llz render`
  and `llz doctor` reject it.
- **Isolated blast radius.** A broken manifest degrades only its own namespace's
  Application. It cannot wedge the platform bootstrap, and it cannot affect your
  other namespaces.
- **Nothing is deleted behind your back.** Generated Applications sync with
  `prune: false`, and the ApplicationSet sets `preserveResourcesOnDeletion: true` —
  so removing a directory from git orphans its resources rather than tearing down
  running workloads. Deleting is deliberate: `kubectl delete` what you mean to.
- **Pinned with the platform.** The generated Applications track your instance's
  `apps_repo_revision`, the same revision `platform-bootstrap` uses.

## Helm / OCI charts

Drop an Argo CD `Application` pointing at a chart into the right directory. It rides
the permissive `instance-custom` AppProject (`sourceRepos: '*'`), so any chart repo
works. Pin the chart version in your Application — that's your source of truth, not
a branch. Argo CD `Application` objects live in the `argocd` namespace, so put them
under `namespaces/argocd/`:

```yaml
# custom/namespaces/argocd/my-helm-app.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-helm-app
  namespace: argocd
spec:
  project: instance-custom
  source:
    repoURL: <your chart repo>
    chart: <chart>
    targetRevision: <pinned version>
  destination:
    server: https://kubernetes.default.svc
    namespace: my-app
  syncPolicy:
    automated: { prune: true, selfHeal: true }
```

For the full contract, see **`docs/extending-llz.md` → "Your own Kubernetes
resources"** in the template repo. (It is deliberately not copied into your
instance — `llz ci deliver-docs` keeps only quickstart + runbooks + playbooks
locally. `docs/README.md` carries the version-pinned link to the rest.)
