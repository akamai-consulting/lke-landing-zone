# llz-argo-bootstrap-apps

App-of-apps **generator**. Renders the ordered set of bootstrap Argo CD
`Application` CRs (and, optionally, the `AppProject` they live in) from a
values-driven `components` list. The cluster's hard-won **sync-wave ordering is
encoded as the default `components` list**, so a sibling system team gets a
correctly-ordered bootstrap by setting values instead of hand-authoring
Application YAML.

## Why this chart exists

The bootstrap path is deterministic only because the Applications sync in a
specific order: the `AppProject` (wave -20) must exist before any Application
that references it; Argo Events (-15) must register the
`EventBus` CRD before llz-cert-automation (-14) references it; stateful OpenBao (0)
consumes the CA chain laid down earlier. That ordering is operational knowledge
that previously lived only in a pile of per-component `argocd/applications/`
wrapper files. This chart captures it as **defaults** so the next adopter
doesn't re-discover it.

> **Not wired into the live tree.** The existing repo manages its Applications
> via the `platform-apl/manifest/kustomization.yaml` resource list. This chart is the
> *generator* a NEW system team uses instead — it is intentionally standalone.

## How a sibling team adopts it

1. Set `global.gitRepoURL` to **their** platform repo. It ships as a
   `REPLACE_ME-…` placeholder; the chart still renders with it (so `helm
   template` with defaults is clean) but stamps a loud `⚠️` warning comment next
   to every `git`-sourced Application until it's overridden.
2. Toggle components on/off via each entry's `enabled`.
3. Adjust waves / chart versions / namespaces only where their stack differs.

```sh
helm template our-bootstrap oci://ghcr.io/akamai-consulting/charts/llz-argo-bootstrap-apps \
  --version 0.1.22 \
  --set global.gitRepoURL='git@github.com:yourorg/yourplatform.git' \
  --set 'components[N].enabled=false'   # disable a component you don't run
```

The output is one `Application` per enabled component, each with the right
`argocd.argoproj.io/sync-wave` annotation, `project`, `source`, `destination`,
and `syncPolicy`. Apply it (or point a root app-of-apps at it) and Argo CD syncs
the components in wave order.

### Turning a component off

Each component is a list entry; `enabled: false` drops its Application from the
render. Either set it inline in a values file or via `--set`:

```yaml
# values-override.yaml
global:
  gitRepoURL: git@github.com:yourorg/yourplatform.git
components:
  - name: <component-name>
    enabled: false   # this team doesn't run this component
```

(With `--set`, address the entry by list index, e.g.
`--set 'components[3].enabled=false'` (indices are 0-3).)

## Default components and their waves

| Component | Wave | Source | Why this wave |
|---|---|---|---|
| `llz-cluster-foundation` | -20 | oci (GHCR first-party) | Creates the `llz-*` namespaces and the default-deny NetworkPolicies, so it must land before anything targets them. |
| `llz-argo-workflows` | -15 | oci (argo-helm) | Provides the Workflow/CronWorkflow CRDs. |
| `llz-argo-events` | -15 | oci (argo-helm) | Provides the EventBus/EventSource/Sensor CRDs; must precede `llz-cert-automation`. |
| `llz-cert-automation` | -14 | oci (GHCR first-party) | After `llz-argo-events` (-15) registers the EventBus CRD it references. |

Two components this chart used to render are deliberately gone, and re-adding
them breaks things:

- **`external-secrets-operator`** — apl-core 6.x ships ESO itself. A second
  controller fights apl-core's over the same CRs.
- **`openbao`** — the OpenBao Application is owned elsewhere; an `llz-openbao`
  App rendered here collides with it.

## Values

### Global

| Key | Default | Description |
|---|---|---|
| `global.argocdNamespace` | `argocd` | Namespace the Application/AppProject CRs are created in. |
| `global.destinationServer` | `https://kubernetes.default.svc` | Cluster the Apps deploy into. |
| `global.project` | `platform-support` | AppProject every Application is placed in. |
| `global.chartsRegistry` | `ghcr.io/akamai-consulting/charts` | OCI registry for first-party charts; default for `oci` components without an explicit `repoURL`. |
| `global.gitRepoURL` | `REPLACE_ME-git-repo-url` | Platform git repo for `git` components + AppProject sourceRepos. **Must be overridden** — renders with a `⚠️` warning comment until set. |
| `global.targetRevision` | `main` | Default git revision for `git` components that don't pin their own. |

### Policy / toggles

| Key | Default | Description |
|---|---|---|
| `defaultSyncPolicy` | see values.yaml | syncPolicy applied to every Application unless a component overrides it (deep-merged). |
| `finalizer` | `true` | Add `resources-finalizer.argocd.argoproj.io` to each CR. |
| `revisionHistoryLimit` | `3` | `spec.revisionHistoryLimit` on each Application. |
| `appProject.enabled` | `false` | Render the AppProject too (off — the live repo already ships it). |
| `appProject.*` | see values.yaml | sourceRepos/destinations/whitelist when `appProject.enabled=true`. |

### Component entry shape

```yaml
- name: <app-name>            # Application metadata.name (no platform- prefix)
  enabled: true               # false drops the Application from the render
  syncWave: "-15"             # sync-wave annotation (string)
  destinationNamespace: <ns>  # spec.destination.namespace
  source:
    type: oci                 # oci | git
    # --- oci ---
    chart: <chart>            # OCI chart name
    version: <semver>         # spec.source.targetRevision
    repoURL: <registry>       # optional; defaults to global.chartsRegistry
    releaseName: <name>       # optional; defaults to component name
    valuesObject: {}          # optional inline Helm values
    # --- git ---
    path: <repo/path>         # in-repo path
    repoURL: <git-url>        # optional; defaults to global.gitRepoURL
    targetRevision: <ref>     # optional; defaults to global.targetRevision
    helm: {}                  # optional helm block (releaseName/valueFiles/...)
  syncPolicy:                 # optional; deep-merged over defaultSyncPolicy
    automated:
      prune: false            # e.g. stateful components disable prune
```

## Validation

```sh
helm lint --strict kubernetes-charts/llz-argo-bootstrap-apps \
  --set global.gitRepoURL='git@github.com:yourorg/yourplatform.git'
helm template llz-argo-bootstrap-apps kubernetes-charts/llz-argo-bootstrap-apps \
  --set global.gitRepoURL='git@github.com:yourorg/yourplatform.git'
```

Every rendered `Application` includes the required `project`, `source`, and
`destination` fields so it passes the repo's `kubeconform` Application
validation.
