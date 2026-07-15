# ADR: apl-core writes to a per-env branch (`apl-<env>`), never `main`

**Status:** Accepted — implemented on the branch that carries this file. Default
changed in `llz render`; existing instances cut over on their next `copier update`
+ render (see Migration). Full runtime validation is gated on the release-e2e run
for this change (see Lab-validation).

**Date:** 2026-07-15

**Relates to:** [apl-core-v6-migration.md](apl-core-v6-migration.md),
[blast-radius-decomposition.md](blast-radius-decomposition.md),
`tools/internal/clusterspec/values.go` (the default),
`instance-template/apl-values/_shared/values.yaml` (the `otomi.git` block),
`.github/workflows/release-e2e.yml` (the retired `e2e-apps` mirror).

## Context

apl-core 6.x runs in BYO-git mode: `otomi.git.repoUrl` points at the external
instance repo, and **apl-operator continuously commits its rendered values tree
back to `otomi.git.branch`** on every reconcile. That tree is a fixed top-level
`env/` directory (the schema forbids `otomi.git.path`) containing per-app state,
CNPG database defs, and — on v6 — **apl-core's own platform secrets as
SealedSecrets** under `env/manifests/namespaces/apl-secrets/sealedsecrets/`. This
was verified live: the operator commits as `x-access-token <pipeline@cluster.local>`
with `otomi commit` / `updated values [ci skip]` messages, and the `env/` tree is
present in the example instance repo.

Until now `otomi.git.branch` defaulted to **`main`** — the same branch that holds
the human-authored Terraform + `apl-values` source, receives `copier update`s, and
is the base for every PR. Sharing `main` between a force-pushing operator and the
IaC source is wrong on several counts:

- **It already caused a production wedge.** apl-core's operator force-pushes a
  divergent history (no common ancestor); when the landing zone's own
  `platform-bootstrap` app-of-apps read the same `main`, it synced a stale,
  operator-authored commit — v1beta1 ExternalSecrets that 6.x's ESO rejects —
  and convergence hard-failed. The e2e worked around this by pointing the LZ's
  apps at a separate `e2e-apps` snapshot branch and *leaving apl-core on main*
  (see [release-e2e.yml](../../.github/workflows/release-e2e.yml)).
- **`main` cannot be branch-protected** while a bot force-pushes it every reconcile.
- **Human PRs / `copier update` race the operator's force-push** on the same ref.
- **Secret material (even sealed) lands on the IaC repo's primary branch.**
- Both `otomi.git.branch` and `apps_repo_revision` defaulted to `main`, so a
  *default* real install reproduces the exact wedge condition — the e2e only
  dodges it by overriding.

The e2e's mitigation (move the *LZ apps* off main) treats the symptom. The cause is
apl-core writing to `main` at all.

## Decision

**`llz render` defaults `otomi.git.branch` to a per-env, apl-core-owned branch named
`apl-<env>`** (e.g. `apl-lab`, `apl-e2e`, `apl-primary`, `apl-secondary`), never
`main`.

- `main` stays human-owned: IaC + `apl-values` source, PR-only, branch-protectable.
- `apl-<env>` is machine-owned: apl-core's `env/` tree + platform SealedSecrets,
  force-pushed freely, read only by apl-core's own `gitops-*` Applications.
- **Each env gets its own branch** so parallel envs (lab / primary / secondary HA
  pairs) never share apl-core state on one branch — a per-env blast radius,
  consistent with [blast-radius-decomposition.md](blast-radius-decomposition.md).
- Overridable per env via `spec.cluster.bootstrap.aplValues.revision`.

The change is one line of intent in
[values.go](../../tools/internal/clusterspec/values.go) (`branch = "apl-" + env`);
`otomi.git.branch` is written *only* by `llz render` into the committed
`values.yaml` — it is not a Terraform variable and no bootstrap resource consumes
it — so there is no other wiring to touch.

## Why this is safe to point at a fresh branch

apl-operator does not require the branch to pre-exist. Per its `EXECUTION_FLOW`,
initial values come from the Helm release (`var.apl_rendered_values`, not a git
clone), and the commit path runs `git checkout -B ${branch}` + `git push -u origin
${branch}` — creating the branch remotely on first reconcile. A fresh `apl-<env>`
is therefore self-created.

The `env/` tree apl-core writes is disjoint from what the landing zone reads:

| Consumer | Reads | Branch |
|---|---|---|
| LZ `platform-bootstrap` + carved Apps | `apl-values/<env>/manifest/` | `apps_repo_revision` (default `main`) |
| apl-core `gitops-*` Apps | `env/manifests/**`, `env/teams/**` | `otomi.git.branch` = `apl-<env>` |

Different top-level dirs *and* different branches — no cross-read, no contention.

## Consequences

- `main` becomes branch-protectable and free of operator force-pushes and sealed
  secret churn.
- apl-core's history is confined to its per-env branch; a wedge on `apl-<env>` can't
  poison the IaC source or another env.
- The e2e's `e2e-apps` snapshot branch is **retired in this change** (main is no
  longer force-pushed by the operator, so `platform-bootstrap` reads `main` directly
  again — `appsRepoRevision` back to `main`, the mirror push and the
  `APPS_REPO_REVISION=e2e-apps` repo-variable set removed).
- Downstream instances need a one-time cutover (Migration).

## Alternatives considered

- **Status quo (`main`).** Rejected — the wedge, no branch protection, secrets on
  the IaC branch.
- **Keep apl-core on `main`, move the LZ apps to a snapshot branch** (what the e2e
  does today). Rejected as the default — it only protects the *readers*; `main` is
  still force-pushed and unprotectable.
- **A single dedicated branch for all envs** (e.g. `apl-values`). Rejected — HA
  pairs and lab/prod would share one operator branch, coupling their blast radius.
- **A separate dedicated values repo per instance** (gold standard for isolation:
  scoped PAT, zero secret material in the IaC repo, IaC-repo protection untouched).
  Not adopted now — more per-instance infra, and `repoURL` is already parameterized
  so it can be layered on later without contradicting this decision. Revisit if
  permission/blast-radius isolation at the repo boundary becomes a requirement.

## Lab-validation checklist (gate before promoting past lab)

- [ ] **Fresh-branch self-creation.** On a clean bootstrap with no `apl-<env>` on
      the remote, confirm apl-operator creates it (`checkout -B` + `push -u`) and its
      `env/` tree materializes there.
- [ ] **`gitops-*` Healthy off `apl-<env>`.** `gitops-global` / `gitops-ns-*` /
      `team-*-values-gitops` go `Synced/Healthy` reading `apl-<env>`, as they did on
      `main` (runs 29374149739 / 29380924847 / 29409532035).
- [ ] **`main` untouched.** No `pipeline@cluster.local` commits land on `main`
      during or after bootstrap.
- [ ] **Force-push semantics.** Confirm the operator's push is confined to
      `apl-<env>` (reconciles the static-analysis "add-only, no `--force`" claim in
      [apl-core-v6-migration.md](apl-core-v6-migration.md) §(b) against the
      EXECUTION_FLOW "force-push every reconcile" description — either way it must
      stay on `apl-<env>`).
- [ ] **release-e2e green** end-to-end with `env=e2e` → `apl-e2e`.

## Migration — downstream instances

Per-instance, in-place (no fresh install):

1. `copier update` to pull the new `llz render` default.
2. `llz render` (or the next bootstrap render) rewrites
   `apl-values/<env>/values.yaml` `otomi.git.branch` → `apl-<env>`.
3. `terraform apply` → the next apl-operator reconcile checks out `apl-<env>`,
   pushes its `env/` tree there, and stops writing `main`.
4. Optional cleanup: delete the now-orphaned `env/` tree from `main` (it is no
   longer read by anything) once step 3 is confirmed.
5. Enable branch protection on `main` (now that no bot pushes it).

Existing instances that intentionally set `aplValues.revision` are unaffected
(explicit value wins over the default).

## Follow-ups

**Done in this change:**

- ✅ Retired the e2e `e2e-apps` snapshot branch — `appsRepoRevision` set back to
  `main`, the mirror force-push and the `APPS_REPO_REVISION=e2e-apps` repo-variable
  set removed (`release-e2e.yml`). Validated by this PR's release-e2e run.
- ✅ Dropped the apl-core-internal gitops deferrals (`gitops-global`,
  `gitops-ns-apl-*`, `team-*-values-gitops`) from the health allowlist — verified
  `Synced/Healthy` on v6 e2e, so the name-based deferral was a no-op for the real
  state (`MatchExternalDep` runs before the Synced+Healthy rule) that only masked a
  genuine `Unknown/ComparisonError` regression. They now pass on their real state and
  a regression surfaces on the gate (`argo_test.go` updated to assert the stricter
  behavior). This PR's release-e2e run confirms they stay `Synced/Healthy` at gate time.

**Open:**

- Evaluate the dedicated-values-repo option if repo-boundary isolation is required.
