# First-party Helm charts

Independently-versioned Helm charts extracted from the cluster's raw kustomize
manifests. Each chart turns per-env kustomize overlays into a documented `values.yaml` and
captures the project's hard-won operational scars as **defaults** — so a sister
system team (Linode LKE-E + apl-core) stands up the same component by setting
values, not editing YAML.

## Charts

| Chart | Replaces | Deploys |
|---|---|---|
| [`llz-cluster-foundation`](llz-cluster-foundation/) | `foundation/` | Secure-by-default baseline: namespaces, default-deny NetworkPolicies, CoreDNS, storage-class defaulting |
| [`llz-openbao-platform`](llz-openbao-platform/) | `openbao/argocd/applications/` | Opinionated OpenBao-on-K8s wrapper (TLS, NP, ServiceMonitor, audit shipping) |
| [`llz-cert-automation`](llz-cert-automation/) | `llz-cert-automation/` | Event-driven cert renewal (Argo Events + Workflows) |
| [`llz-argo-bootstrap-apps`](llz-argo-bootstrap-apps/) | per-component `argocd/applications/` wrappers | App-of-apps generator encoding sync-wave ordering |

> **Out of scope — product workloads.** The reusable unit is the *platform*, not
> the product. A sibling team's application workloads are product-specific and are
> **not** chartified here; they bring their own product workloads.

## Distribution

Charts publish to **GHCR** as OCI artifacts:

```
oci://ghcr.io/akamai-consulting/charts/<chart>:<version>
```

GHCR (not the in-cluster Harbor) is deliberate: the charts the bootstrap itself
consumes must come from a registry that exists *before* the cluster does
(avoids the Harbor chicken-and-egg).

[`.github/workflows/publish-charts.yml`](../.github/workflows/publish-charts.yml)
packages and pushes every chart whose `version:` is not already published.
Versioning is **immutable by convention**: to release a change, bump the chart's
`Chart.yaml` `version:` — never overwrite an existing tag, because the monorepo's
Argo Applications pin `targetRevision: X.Y.Z`.

## Consumption (the monorepo dogfoods its own charts)

A chart is consumed by an Argo CD `Application` that references the OCI artifact
rather than an in-repo path. Today exactly one lives in this repo:
[`platform-apl/components/openbao/openbao.yaml`](../platform-apl/components/openbao)
(`chart: llz-openbao-platform`). That consumer relationship is the forcing
function that keeps an extracted chart honestly reusable.

### Cutover status

**Only `llz-openbao-platform` is consumed live from this repo.** The other three are
published and reusable, but nothing here deploys them — read the table as "what is
dogfooded", not "what is deployed".

| Chart | Live consumption here | Notes |
|---|---|---|
| `llz-openbao-platform` | ✅ `platform-apl/components/openbao/openbao.yaml` | HA-Raft boots fresh on a recreated cluster. `releaseName: platform-openbao` is preserved (StatefulSet/cert/raft identity); `OPENBAO_CHART` Makefile targets + `replacements:` repointed accordingly. |
| `llz-cluster-foundation` | ❌ not deployed on managed | Namespaces, default-deny NPs, CoreDNS, storage-class defaulting. On Managed App Platform apl-core owns all of it, so the component is `ManagedSkip` — and managed is the only supported mode (ADR 0005). Still published for a self-installed consumer. |
| `llz-cert-automation` | ❌ not deployed on managed | Event-driven cert renewal. Same reason: cert-automation is apl-core's on managed. |
| `llz-argo-bootstrap-apps` | n/a (generator) | Standalone app-of-apps generator for a *new* sibling team; intentionally not wired into this repo's kustomization. |

> **Known stale references in `components.go`.** `clusterFoundation` and several
> other component entries still carry `ArgoApps: []string{"applications/…"}` paths
> under `platform-apl/manifest/applications/`, **a directory that no longer exists**.
> They are unreachable rather than broken: every one of them is `ManagedSkip`, so
> the render never resolves them on a managed cluster. Anyone re-enabling a
> self-installed path must restore those manifests first.

> **Rollout ordering (hard prerequisite).** Before bootstrapping the recreated
> cluster, the charts must be **published to GHCR** (the `publish-charts` workflow
> runs on merge) and the private GHCR OCI registry registered as an Argo CD Helm
> repo (and listed in the `platform-support` AppProject `sourceRepos` — already
> added). On greenfield this is a clean one-time ordering: publish, then bootstrap.

## Conventions

- **No `platform-`/org prefix** on chart resource names or release names — names are
  generic so two system teams don't collide.
- **Scars as defaults.** Every non-obvious value (NetworkPolicy CIDRs, sync-wave
  ordering, singleton update strategy, RBAC narrowing) ships as a default with a
  comment explaining the failure mode it prevents.
- **`helm lint --strict` + `helm template` clean** for every chart — enforced by
  `make helm-lint-charts` and the `helm-lint-charts` CI step.
- Linode + apl-core assumptions stay as **defaults** (not abstracted); only
  org/cluster identity (endpoints, domains, CIDRs, names) is variabilized.
