# Proposal: declarative LandingZone spec — single resource vs. shared + 1-per-env

**Status:** Draft for discussion · **Scope:** representation/shape only (not the renderer mechanics)

## Context

Today a single LLZ instance is configured across several disjoint places an
operator must keep mutually consistent by hand:

1. `.copier-answers.yml` — instance identity (`upstream_org`, `instance_repo`, `forge_flavor`, `llz_version`)
2. `cluster/<env>.tfvars`, `cluster-bootstrap/<env>.tfvars`, `object-storage/<env>.tfvars` — per-env cluster definition
3. `apl-values/<env>/manifest/` — the kustomize tree selecting which components ("recipes") deploy

We want to collapse the human-facing surface into a **declarative,
Kubernetes-resource-shaped spec** (`apiVersion`/`kind`/`metadata`/`spec`) that
the `llz` CLI reconciles into the files above — CLI-rendered now, but shaped so it
can graduate to a real CRD + controller later without a rewrite.

This proposal is **only about how that spec is laid out on disk**. Two shapes are
on the table. Runnable examples accompany each:

- **Option A — single resource:** [`examples/single-resource/llz.yaml`](examples/single-resource/llz.yaml)
- **Option B — shared + 1-per-env:** [`examples/split-resources/landingzone.yaml`](examples/split-resources/landingzone.yaml)
  + [`examples/split-resources/clusters/prod.yaml`](examples/split-resources/clusters/prod.yaml)
  + [`examples/split-resources/clusters/staging.yaml`](examples/split-resources/clusters/staging.yaml)

## Option A — single resource

One repo-root `llz.yaml` (`kind: LandingZone`) holds the instance identity **and**
every environment inline under `spec.environments.<name>`.

```yaml
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: platform-support }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: my-org/platform-support, forge: github, templateVersion: v0.4.0 }
  environments:
    prod:    { cluster: { … }, recipes: { … } }
    staging: { cluster: { … }, recipes: { … } }
```

**Strengths**
- One file, one source of truth; the whole instance is reviewable at a glance.
- Simplest tooling: one document to parse, no cross-file references or inheritance.
- No defaulting indirection — what you see in the file is what renders.
- Best onboarding for a small (1–2 env) instance.

**Weaknesses**
- The file grows with every environment; diffs touch a large shared document.
- Editing one env produces a diff in the file that owns *all* envs → noisy review and merge conflicts when two people change different envs.
- Coarser ownership: `CODEOWNERS` can gate the file, not an individual env.
- Larger blast radius: a malformed edit (bad YAML, wrong indent) can fail parsing for *every* environment at once.
- Poor CRD fidelity: an in-cluster CRD is one object per resource. A single object carrying a map/list of environments is an awkward "list-in-spec" shape that does **not** map cleanly to a future `ClusterDefinition` CR.

## Option B — shared + 1-per-env

A shared `landingzone.yaml` (`kind: LandingZone`) holds the instance identity plus
**optional shared defaults**, and each environment is its own
`clusters/<env>.yaml` (`kind: ClusterDefinition`, `metadata.name == env`) that
inherits the defaults and overrides only what is env-specific.

```yaml
# landingzone.yaml
kind: LandingZone
spec:
  instance: { … }
  defaults:                        # inherited by every ClusterDefinition
    cluster: { k8sVersion: v1.33.6+lke7, nodePool: { type: g8-dedicated-8-4, count: 5 } }
---
# clusters/prod.yaml
kind: ClusterDefinition
metadata: { name: prod }
spec:
  cluster: { clusterLabel: platform-prod, region: us-ord, … }   # k8sVersion/nodePool inherited
  recipes: { harbor: { enabled: true } }
```

**Strengths**
- Diff/PR locality: a change to `staging` touches only `clusters/staging.yaml`.
- Fine-grained ownership: `CODEOWNERS` per `clusters/*.yaml` (prod gated by SREs, lab self-serve).
- Smaller blast radius: a bad env file fails that env; the others still parse and render.
- DRY across a fleet: shared `defaults` set the common case once; envs override the exception (see staging dropping `harbor`, shrinking `nodePool`).
- High CRD fidelity: this **is** the in-cluster shape — one `LandingZone` object + one `ClusterDefinition` object per env. Graduating to a controller is a near-mechanical lift.
- Matches today's discovery model: CI already globs `cluster/*.tfvars`; globbing `clusters/*.yaml` is the same pattern.

**Weaknesses**
- More files and a directory convention to learn.
- Inheritance has to be specified and implemented (merge semantics: per-env overrides shared overrides built-in defaults). More tooling surface and a subtle place for bugs.
- The instance is no longer viewable in a single file; you read the shared file plus the env file to know an env's effective config.
- Slightly more ceremony for a trivial single-env instance.

## Comparison

| Axis | A — single | B — shared + per-env |
|---|---|---|
| Files | 1 | 1 + N |
| Diff/review locality | whole-instance file | per-env file |
| Merge-conflict surface | high (shared file) | low (isolated files) |
| Per-env ownership (CODEOWNERS) | no | yes |
| Blast radius of a bad edit | all envs | one env |
| DRY across many envs | repeat per env | shared `defaults` + override |
| Discovery | read one file | glob a dir (matches tfvars today) |
| Tooling complexity | low (one parse) | medium (parse + inherit/merge) |
| Single-file readability | full | partial (shared + env) |
| **CRD-graduation fidelity** | **low** (list-in-spec) | **high** (one object per resource) |

## Recommendation

**Adopt Option B (shared + 1-per-env).** The deciding factors are the two that
compound as the project grows:

1. **CRD fidelity.** The stated end-state is a real CRD + controller. Option B is
   already that shape (`LandingZone` + per-env `ClusterDefinition`); Option A would
   have to be reshaped to get there. Choosing B now avoids a second migration.
2. **Operational locality.** Per-env files give scoped diffs, per-env `CODEOWNERS`,
   and a blast radius of one — which matter precisely when an instance has the
   several prod/staging/lab/HA-pair environments this template targets.

Mitigate Option B's main cost (DRY/indirection) with the shared `spec.defaults`
block, so common settings live once and env files stay short.

Keep Option A in mind as a documented "simple mode": a 1-environment instance can
start with a single `llz.yaml` and split later — the schemas are compatible
(`LandingZone.spec.environments.<env>` ≅ a `ClusterDefinition.spec`).

## Open questions

- **Inheritance depth.** Is one `defaults` layer enough, or do we want env *groups*
  (e.g. a shared "prod-like" profile) between `defaults` and a single env?
- **Cross-file validation.** HA pairing (`active`/`standby` sharing a group) and
  unique `promotionRank` span multiple env files — the validator must load the
  whole `clusters/` set, not one file. Confirm that is acceptable.
- **Reference style.** Implicit single-`LandingZone`-per-repo (assumed here) vs. an
  explicit `landingZoneRef` on each `ClusterDefinition`. Implicit is simpler; explicit
  is needed only if one repo ever hosts multiple instances (it does not today).
- **Naming.** `ClusterDefinition` vs. `Environment` vs. `Deployment` for the per-env kind.

## Non-goals

The renderer mechanics — how the spec becomes tfvars (transient, build-time) and
the committed recipe kustomizations (Argo syncs committed git), plus the
`.copier-answers.yml` sync — are out of scope here and unchanged by the A-vs-B
choice. This proposal decides only the on-disk shape.
