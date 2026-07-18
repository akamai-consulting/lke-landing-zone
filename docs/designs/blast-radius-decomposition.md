# Blast-radius decomposition of platform-bootstrap

## Problem

`platform-bootstrap` is a hybrid app-of-apps. Some pieces are already independent
Argo CD `Application`s that health-gate their own content (`instance-custom`,
`llz-openbao`, `llz-secret-store`), but four toggleable bundles still render as raw
kustomize **Components** merged into the platform-bootstrap tree, sharing its single
sync/health fate:

| component        | content                                                        |
|------------------|----------------------------------------------------------------|
| `externalSecrets`| platform default-deny + ESO-egress NetworkPolicies             |
| `observability`  | otel-collector, loki object-store ExternalSecret, dashboard, rules, otel CA |
| `harbor`         | robot-provisioner CronJob + ExternalSecrets + mesh NetworkPolicy |
| `llzReconciler`  | the day-2 `llz reconcile` Deployment + RBAC/netpol/ServiceMonitor/rules |

Because they merge as raw resources, one `Degraded` resource in any of them fails the
whole platform-bootstrap sync — the #142 (health-wedge) / #163 (wave-dependency-wedge)
class. A single stuck resource strands every unrelated resource sharing that fate.

## Mechanic

`instance-custom`, `llz-openbao`, and `llz-secret-store` are the proven template: a
git-path `Application` whose CR is health-inert in the parent tree (Argo assesses no
`Application` health unless an explicit customization is configured, which apl-core
does not), so platform-bootstrap stays `Synced/Healthy` while the App health-gates its
OWN content. A `Degraded` resource then fails only its own App.

> **Update:** `instance-custom` has since taken this one cut further — it is now an
> `ApplicationSet` generating one App per `kubernetes-custom/namespaces/<ns>/` directory
> (plus `global/`), so the operator's escape hatch has per-namespace blast radius for its
> CONTENT rather than a single shared fate.
>
> **But the health-inertness argument above does NOT transfer to it.** A plain
> `Application` is health-inert; an `ApplicationSet` is not — Argo ships
> `resource_customizations/argoproj.io/ApplicationSet/health.lua`, which reports Degraded
> on an `ErrorOccurred` condition (generation errors, validation errors, create/update
> failures). Bad manifest content still degrades only the generated App, but a generation
> or validation error degrades the ApplicationSet itself and thus platform-bootstrap's
> health rollup. The operator-triggerable causes are directory names that yield an
> invalid Application/namespace, which `llz render`/`llz doctor` reject at render time
> (`tools/cmd/llz/custom_layout.go`). That render-time gate is weaker than the VAP-style
> admission enforcement this document argues for elsewhere — a direct commit bypasses it.
> Closing that gap is open work. See docs/extending-llz.md.

This PR generalizes that to the four bundles. When `spec.components.<name>` is enabled
and the component declares a `CarvedApp` (registry field in
`tools/internal/clusterspec/components.go`), `llz render` emits:

1. a health-inert `Application` CR `llz-<name>` into `apl-values/<env>/manifest/`
   (referenced under `resources:` of the thin overlay, replacing the old
   `components: ../../components/<name>` entry);
2. a **self-contained, env-correct source root** at `apl-values/<env>/apps/<name>/`:
   a `kustomization.yaml` that pulls in the shared Component
   (`../../../components/<name>`, which lives ONCE on disk) and applies this env's
   patches (e.g. observability's `otel.<env>.internal` SAN, the reconciler's
   `REGION_SHORT`) — so the App's `spec.source.path` has everything it needs.

The App CR pins the same instance repo + `apps_repo_revision` the platform-bootstrap
Application uses (`ValuesIdentity.RepoURL`), under `project: platform-bootstrap` (whose
AppProject is already fully permissive).

## Sync-wave correctness

Argo waves gate resources **within** one App; **across** Apps the app-of-apps waves
gate the App CRs (which apply near-instantly and are health-inert — there is no
cross-App health gate). So each carved App carries an App-level `sync-wave` that is the
FLOOR for its content, and the dependency root gets the lowest so it comes up first:

| carved App           | App wave | rationale                                             |
|----------------------|----------|-------------------------------------------------------|
| `llz-externalsecrets`| **-10**  | dependency root — its netpols gate whether ANY ExternalSecret can reach OpenBao |
| `llz-observability`  | -5       | leaf; nothing depends on its health                   |
| `llz-harbor`         | 5        | content all at wave 5 (after the openbao store)       |
| `llz-reconciler`     | 5        | Deployment (6) consumes its own ExternalSecret (5)    |

Chain: `llz-secret-store` → `llz-externalsecrets` → consumers.

### The guard must gain cross-Application awareness

`llz ci wave-dependency-guard` (the #163 gate) used to compare resource sync-waves in
one flat tree. Post-carve that goes blind to the exact class it exists to catch: a
workload in one App whose Secret is produced by an ExternalSecret in a **later-created**
App. The guard now reads the SAME `CarvedApp` registry the renderer does and judges
ordering by topology:

- **same Application** → compare RESOURCE sync-waves (unchanged; Argo orders a single
  App's resources by them);
- **different Applications** → compare APP-level waves (a carved App's content cannot
  sync before the app-of-apps creates it, and siblings race). The workload's App must
  be created strictly after the ExternalSecret's App.

The platform-bootstrap root tree ranks earliest (created by Terraform before any child
App), so a root workload depending on a carved ExternalSecret is flagged, while a carved
workload depending on a root ExternalSecret is fine.

`wave-health-guard` (the #142 gate) needed no logic change: it already treats
`argoproj.io/Application` at any wave as health-inert, so the carved App CRs (some at
negative waves) pass.

## #3 — Admission-time enforcement (VAP)

The CI guards run on the rendered tree at PR time, but the apl-operator force-pushes
values out-of-band — a non-CI change (operator writeback, direct SSA) can still land a
wave violation. `llz-wave-health-guard`, a native `ValidatingAdmissionPolicy`
(`platform-apl/manifest/admission/`), rejects the wave-HEALTH class at admission
regardless of write path, with zero new controllers.

The wave-health invariant is per-object (kind + wave), which maps cleanly onto CEL; the
wave-DEPENDENCY invariant is cross-object (a workload vs the ExternalSecret feeding it)
and stays CI-only — a VAP cannot see sibling resources. The policy is scoped via
`matchConditions` to objects that carry the sync-wave annotation, so even with
`failurePolicy: Fail` a CEL fault cannot wedge unrelated admissions. Its inline CEL
allowlist is pinned to the Go guard's `waveHealthAllowedKinds` / `waveHealthAllowedNames`
by `TestWaveHealthVAPMatchesGuard` — add a vetted kind in both places or the build fails.

**Scope caveat (found by a live negative-wave census).** The CI guard scans only the
platform-bootstrap kustomize tree; the VAP sees every admission cluster-wide. A census
of a converged cluster found `argoproj.io` CRs at negative waves that are NOT in that
tree — `Sensor`/`EventSource`/`EventBus` (wave -14, managed by the `llz-cert-automation`
child App, which health-gates its own content) — plus `Workflow`s. Denying those would
wedge their Apps. Every historical wedge class is non-argoproj (#142 NetworkPolicy /
ClusterIssuer, #163 Deployment), and Argo's own `Application`/`AppProject` are
health-inert, so the VAP excludes the whole `argoproj.io` group. This keeps the VAP's
effective coverage equal to the CI guard's (non-argoproj platform-tree kinds) without
false-denying child-App CRs. `Application`/`AppProject` stay in the allowlist for
lockstep with the CI guard, which does scan them in `platform-apl/manifest`.

## #4 — Wedge game-day

The containment claim is proven by fault injection (`llz ci wedge-gameday`): force one
platform ExternalSecret NotReady and assert **only** `llz-externalsecrets` degrades
while `platform-bootstrap` and the sibling carved Apps stay `Healthy` — the concrete
proof the #163 blast radius is contained. Run on the warm cluster the converge-only
fast-path (#146) reuses.

## Non-goals

The operator Apps (already split) and the low-churn glue that stays in-bundle —
kyverno-policies, dns, generated-secrets, AppProjects, cluster-foundation — are left
alone. Carving them buys no isolation their low churn doesn't already give.
