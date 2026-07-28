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
kubernetes-custom/
  namespaces/<namespace>/    # namespaced resources → synced INTO <namespace>
  global/                    # cluster-scoped resources (CRDs, ClusterRoles, ...)
```

- **One Argo CD Application per `namespaces/<ns>/` directory.** The namespace is
  created automatically if it does not exist.
- **Subdirectories are organizational only** — everything under a namespace
  directory is recursed and applied into that namespace.
- **No kustomize here.** The generated Applications use directory recursion, and Argo
  cannot do both — an explicit directory source disables its kustomize
  auto-detection, so a `kustomization.yaml` would be applied to the cluster as a
  literal `kind: Kustomization` object rather than built. `llz render` / `llz doctor`
  reject one. If you want kustomize, drop your own Argo CD Application pointing at
  your kustomize root (see "Helm / OCI charts" below — same route, any source repo).

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
  Application. It cannot affect your other namespaces, and it cannot degrade the
  platform bootstrap. (A directory *name* Kubernetes would reject is the one thing
  that can — the ApplicationSet reports an error rather than the App. `llz render`
  and `llz doctor` catch those names before they reach a cluster.)
- **Nothing is deleted behind your back.** Removing a directory from git **orphans**
  its resources — the Application goes away, the running workloads stay. Deleting is
  deliberate: `kubectl delete` what you mean to. (The ApplicationSet's
  `preserveResourcesOnDeletion: true` is what buys this; `prune: false` governs
  something else — resources removed from a directory that still exists.)
- **Live, not pinned.** The generated Applications track the default branch, so
  dropping a file in applies it — even when your platform is pinned to a release
  tag. The trade: there's no pin to roll back to, so that branch's PR review is the
  gate. Deliberate; see `docs/extending-llz.md` in the template repo.

## Secrets: put them under a team subtree

Your `ExternalSecret`s resolve through the platform's `openbao` ClusterSecretStore,
whose read identity is **not** an all-of-OpenBao grant. It can read:

- a fixed **platform** allowlist (`harbor/*`, `linode/*`, `infra/*`, … — LLZ's own
  paths, and not yours to add to from here), plus
- the subtree of **every team declared in `spec.teams`** (`openbaoSubtree`, e.g.
  `secret/platform`), which `llz ci bao-configure` grants a reader policy on.

So a `remoteRef.key` outside those — `myapp/db-password`, say — is a **403 at sync
time**, not a missing-path error, and the Secret is simply never created. Downstream
pods then sit in `CreateContainerConfigError` with nothing pointing back at OpenBao.
Name your paths under a declared team's subtree and the grant already covers them:

```yaml
apiVersion: external-secrets.io/v1     # v1, NOT v1beta1 — apl-core v6's ESO
kind: ExternalSecret                   # stopped serving v1beta1 (`llz lint` fails on it)
metadata:
  name: myapp
  namespace: my-app
spec:
  secretStoreRef: { name: openbao, kind: ClusterSecretStore }
  target: { name: myapp, creationPolicy: Owner }
  data:
    - secretKey: db-password
      # secret/platform/* — the `platform` team's subtree, which ESO can read.
      remoteRef: { key: platform/myapp, property: db-password }
```

Seed it with the team credential, not a root token:

```console
$ eval "$(llz openbao login --team platform)"
$ llz openbao set secret/platform/myapp db-password=... --yes
```

Declaring a team **later** does not retroactively grant the reader policy — that
needs a re-configure. See `docs/runbooks/openbao-team-login.md` ("Retrofit …").

> **Confidentiality caveat:** the per-team reader policies sit on ONE shared ESO
> identity, so any namespace's ExternalSecret can read any team's subtree. The
> subtree split is about *reachability*, not isolation. The **write** side is
> properly per-team (each `<name>-writer` binds that team's own Keycloak group).

## Helm / OCI charts

Drop an Argo CD `Application` pointing at a chart into the right directory. It rides
the permissive `instance-custom` AppProject (`sourceRepos: '*'`), so any chart repo
works. Pin the chart version in your Application — that's your source of truth, not
a branch. Argo CD `Application` objects live in the `argocd` namespace, so put them
under `namespaces/argocd/`:

```yaml
# kubernetes-custom/namespaces/argocd/my-helm-app.yaml
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
