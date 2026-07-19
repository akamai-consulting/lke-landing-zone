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

Each cut-over chart is consumed by an Argo CD `Application` that references the
OCI chart instead of an in-repo path — `platform-apl/manifest/applications/cluster-foundation.yaml`
for the foundation chart, and the `platform-apl/components/` kustomize components
(`components/openbao/openbao.yaml`, `components/certManager/cert-automation.yaml`)
for the other two. This consumer relationship is the forcing
function that keeps the extracted charts honestly reusable.

### Cutover status

Three of the four platform charts are **consumed live** via OCI Argo Applications (the
monorepo dogfoods its own published charts). `llz-argo-bootstrap-apps` is a
standalone generator. The cluster is rebuilt greenfield, so there is no live
state to migrate — the bootstrap stands the whole platform up from the charts.

| Chart | Live consumption | Notes |
|---|---|---|
| `llz-cert-automation` | ✅ OCI Argo App | CRDs handled via `SkipDryRunOnMissingResource`. |
| `llz-cluster-foundation` | ✅ OCI Argo App | Namespaces/NPs/Jobs; wave-`-20` health-gated so they're Healthy before wave-`-15` consumers. |
| `llz-openbao-platform` | ✅ OCI Argo App | HA-Raft boots fresh on the recreated cluster. `releaseName: platform-openbao` preserved (StatefulSet/cert/raft identity); `OPENBAO_CHART` Makefile targets + `replacements:` repointed/cleaned. |
| `llz-argo-bootstrap-apps` | n/a (generator) | Standalone app-of-apps generator for a *new* sibling team; intentionally not wired into this repo's kustomization. |

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
