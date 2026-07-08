# Design: a reuse pattern that doesn't depend on same-org secret inheritance

**Status:** In progress — design + Phase 0 landing on branch `feat/cross-org-reuse-pattern`.
**Tracks:** [#201](https://github.com/akamai-consulting/lke-landing-zone/issues/201) (this design) · [#200](https://github.com/akamai-consulting/lke-landing-zone/issues/200) (the cross-org `secrets: inherit` bug + guardrail).
**Relates to:** `instance-template/.github/workflows/`, `instance-template/.github/actions/`, `.github/workflows/llz-*.yml` (the reusable workflows), `copier.yml`, `instance-template/.template-manifest`, `tools/cmd/llz/checks.go`.

## Problem

The pipeline is distributed to instances as **thin caller workflows** that delegate the entire body to a reusable workflow in the template org via `uses: … secrets: inherit`. That works only when the instance repo is in the **same organization or enterprise** as the template. For any other adopter org, `secrets: inherit` silently delivers **no** secrets (repo, org, and environment scoped alike) — the pipeline runs with empty credentials and fails with cryptic downstream errors (`No valid credential sources found`, `require-secret … is not set`). Confirmed live: `akamai/gsap-apl` (org `akamai`) → `akamai-consulting/lke-landing-zone` (org `akamai-consulting`).

The documented remedy — "ADOPTERS: repoint the `uses:` org to your fork" — is a per-org workaround, not a fix: every adopter forks and maintains the whole repo, two refs (`uses:` + `template-ref`) must stay in lockstep, forks drift, and forgetting it fails **silently**.

## Why it can't be patched inside the reusable-workflow model

Three GitHub constraints compose into a dead end for cross-org environment secrets:

1. A job that calls a reusable workflow (`uses:` at job level) **cannot also declare `environment:` or `steps:`** — the caller can't enter an environment to resolve env secrets before delegating.
2. **Environment secrets are only readable by the job that declares `environment:`.** With (1), that can only be the reusable's own jobs.
3. Those jobs receive caller secrets only via `secrets: inherit`, which is **same-org/enterprise only**.

Explicit `secrets:` passing doesn't help: the caller's `uses:` job has no `environment:`, so `${{ secrets.X }}` there resolves to repo scope only — env secrets are already unreachable at the call boundary.

## The pattern: local job graph, central step logic

Invert the split:

- **Instance repo owns orchestration** — the jobs, their `environment:`, the matrix, `needs`. These are normal `steps:` jobs, so they read the instance's own repo **and** environment secrets directly. No inheritance, no boundary.
- **Central repo owns logic** — composite actions + the `llz` container image, both consumable cross-org **without secrets** (public GHCR image + composite actions at a pinned tag). Secrets are read in the instance job and passed **explicitly** as `with:` inputs / `env:`; composite actions never declare `secrets:`.

```yaml
# instance repo — copier-rendered, Renovate-synced. A real job, not a uses: delegation.
apply-vpc:
  environment: infra-${{ inputs.region }}                 # env secrets resolve HERE, in-repo
  container: { image: ${{ vars.TF_IMAGE }} }              # central logic, public image
  steps:
    - uses: actions/checkout@<pin>
    - uses: <@ upstream_org @>/lke-landing-zone/instance-template/.github/actions/terraform-init@<@ llz_version @>
      with:
        aws-access-key-id:     ${{ secrets.TF_STATE_ACCESS_KEY }}   # in-job → env secret resolves
        aws-secret-access-key: ${{ secrets.TF_STATE_SECRET_KEY }}
    - run: llz ci tf-apply --region ${{ inputs.region }}            # logic lives in the container/CLI
```

### Why this is a small move, not a rewrite

The pipeline is already ~80% here (verified against the tree):

- Everything runs in the central container image (`vars.TF_IMAGE`); all real work is `llz ci …` subcommands.
- The reusables already check out the template into `_llz-template/` and call its composite actions (e.g. `./_llz-template/instance-template/.github/actions/terraform-init`, [.github/workflows/llz-terraform.yml](../../.github/workflows/llz-terraform.yml)).
- The composite actions already take `with:` inputs, not `secrets:` ([terraform-init/action.yml](../../instance-template/.github/actions/terraform-init/action.yml)).
- The repo is **public**, so GHCR images + composite actions at a pinned tag are consumable cross-org with no auth.
- `copier.yml` delimiters (`<@ @>`/`<% %>`) were chosen specifically so `${{ }}` job graphs pass through untouched, and the `upstream_org` / `llz_version` / `llz_image_ref` tokens already exist.

The **only** thing forcing cross-org inheritance is the outer reusable-workflow wrapper. Removing that layer and promoting its job graph into the instance eliminates the boundary while leaving all logic central. The composite-action reference changes from a local `_llz-template` checkout to the cross-org `uses: <@ upstream_org @>/lke-landing-zone/instance-template/.github/actions/<name>@<@ llz_version @>` form (GitHub resolves any subpath at a ref — no relocation needed), which also **deletes the `_llz-template` checkout step** from every job.

### What moves where

| Concern | Today | Proposed |
|---|---|---|
| Job graph / environments / matrix | reusable workflow (template org) | **instance repo** (copier-rendered, Renovate-synced) |
| Step logic | composite actions + `llz` CLI (already central) | unchanged |
| `llz` binary | baked into `TF_IMAGE` | unchanged — public image, pinned `@vX.Y.Z` |
| Secrets | `secrets: inherit` across orgs ❌ | read in-job from the instance's own repo/env ✅ |
| Upgrades | bump `uses:` ref + `template-ref` | bump image tag + composite-action ref (one version, Renovate) |

## Wrinkles the naive framing misses

1. **Nested reusables must also be inlined.** `llz-terraform.yml` itself calls `uses: ./.github/workflows/llz-bootstrap-openbao.yml` (passes env secrets) and `llz-discover-deployments.yml` (PR-only, no secrets). Any reusable that touches env/cross-org secrets — `bootstrap-openbao`, `secret-rotation`, `cluster-health`, `wedge-gameday` — has the same boundary and must become instance-local jobs too. Secret-free ones (`discover-deployments`, lint/checkov) *may* stay cross-org `uses:` calls, since inheritance is moot with no secrets.

2. **The instance's `managed` surface grows, and the sync mechanism is currently unsound** (see Phase 0). The job graph is exactly the kind of large `managed` file that a downstream `copier update` re-renders wholesale — and a real `v0.0.18→v0.0.24` upgrade of `gsap-apl` committed **unresolved conflict markers** into two `kustomization.yaml` files with no guard catching it. Hardening the sync path is therefore a **prerequisite**, not cleanup.

3. **`.github/actions` `_exclude` + manifest stay as-is.** Actions remain template-internal (not copied to instances) — instances *reference* them cross-org, not carry them. Only the reference path in the workflows changes.

4. **Breaking scaffold change → version + migration.** Bump the template, and have `llz upgrade` / `.template-removals` drop the old thin-caller shape cleanly on `copier update`.

## Phasing

### Phase 0 — harden the sync path (prerequisite, independently valuable)

This design leans on `copier update` / `llz upgrade` to keep a much larger `managed` surface current. That path is demonstrably fragile today, so it is hardened first:

- **Conflict-marker gate** — `llz lint` (and the pre-commit hook) fail on committed `<<<<<<<` / `>>>>>>>` merge markers anywhere in the tree. Converts a silent broken-YAML merge into a blocked commit. *(Landed with this design.)*
- **Manifest ↔ copier consistency** — `llz ci template-manifest` asserts that every `owned` path is actually protected by copier's `_skip_if_exists` / `_exclude` (the `gsap-apl` break was an `owned` file that copier merged anyway because it wasn't skipped). The `managed`/`merge`/`owned` map must be *enforced*, not just documented.

### Phase 1 — the cross-org guardrail (from #200)

- **`llz doctor`** parses each `.github/workflows/*.yml`; if any `uses:` org ≠ the instance repo's own org (from `.copier-answers.yml` `instance_repo`) while `secrets: inherit` is present, it **fails loudly** with the exact "secrets: inherit does not cross organizations" message. Ships ahead of the structural change so it protects everyone still on the thin-caller shape.

### Phase 2 — pilot: convert `terraform.yml`

Convert the one workflow to a full instance-local graph consuming cross-org actions + container; validate against the `akamai/gsap-apl` → `akamai-consulting` cross-org repro from #200 (env secrets resolve; no inheritance).

### Phase 3 — convert the rest + retire the reusables

`bootstrap-openbao`, `secret-rotation`, `cluster-health`, `wedge-gameday`, `promote`; inline secret-bearing nested reusables; keep secret-free ones thin. Add a `llz ci workflows-fresh`-style drift guard so a hand-edited instance graph fails CI rather than silently diverging. Retire the `llz-*.yml` reusables (or keep as an internal library for the template's own e2e). Bump the template major + migration.

## Alternatives considered

- **Fork per adopter (status quo remedy).** Per-org maintenance + silent-failure mode. Rejected as the general answer.
- **Explicit `secrets:` mapping instead of `inherit`.** Doesn't help — the caller's `uses:` job has no `environment:`, so env secrets are unreachable at the boundary regardless.
- **Same-enterprise linkage.** Works cross-org, but is an org-admin dependency outside the template's control; no help for external adopters.
- **Collapse into one container job (no matrix/env jobs).** Loses GitHub-native environment gating/approvals and per-deployment parallelism; only partial.

## Day-2: the secretless in-cluster thin caller (OIDC)

The local-job-graph pattern above keeps GitHub Environment gating but fattens the
instance. For **day-2** flows (health, rotation, audits) there is a strictly
better shape that is cross-org, GitHub-Actions-native, AND slim — because day-2
work runs against a cluster that already exists and already runs the
`llz-reconciler`.

**Pattern:** run the check on a self-hosted runner *inside* the cluster (Actions
Runner Controller) and authenticate with **workload identity** — the runner pod's
ServiceAccount for the kube API, and **GitHub OIDC → OpenBao** (`auth/jwt`, the
`platform-ci` role `llz ci bao-configure` already ships) for any stored secret.
GitHub OIDC tokens are minted per-job under `permissions: id-token: write` and are
**not** subject to the cross-org `secrets: inherit` limitation, so this is
identical for an adopter in a different org. The instance carries a true thin
caller with **no `secrets:` block at all**.

- Primitive: `llz ci openbao-login --role platform-ci` (mint OIDC token →
  `auth/jwt/login` over the ClusterIP → export `OPENBAO_TOKEN`). It is the
  reusable, central building block — logic in the binary (tier 3).
- Prototype: [instance-template/.github/workflows/cluster-health-incluster.yml](../../instance-template/.github/workflows/cluster-health-incluster.yml).

**The honest floor (why this is day-2 only).** A *hosted* runner cannot use this
for the terraform/bootstrap flow: its entry credentials (`TF_STATE_*` for
kubeconfig, `LINODE_API_TOKEN` for the LKE-E ACL) are static — Linode and S3 have
no OIDC federation — and OpenBao is ClusterIP-only, so a hosted runner cannot
reach it to bootstrap from (chicken-and-egg). The entry credential is irreducible
for a hosted runner; only an in-cluster runner removes it. So: **bootstrap flow →
local job graph (tier 3, `llz ci tf-module`); day-2 flow → in-cluster OIDC thin
caller.** Both eliminate cross-org `secrets: inherit`; the second also eliminates
the secrets.

This is the pipeline-abstraction endpoint: day-2 signal is the in-cluster
reconciler (continuous) plus thin OIDC-authenticated triggers (synchronous
gates), and GitHub secrets leave the day-2 surface entirely.

## Non-goals

- Changing what the composite actions or `llz ci …` subcommands *do* — only how they are invoked.
- Moving secrets out of GitHub Environments — the environments stay; the point is to read them in-repo.
