# Forge abstraction

`llz` talks to a git **forge** (GitHub, GitHub Enterprise, GitLab) for every
instance-repo operation: secrets, variables, workflow/pipeline dispatch, repo
creation, and deployment-environment branch locking. Historically these were
scattered `exec.Command("gh", …)` calls across ~10 files. They now funnel through
a single interface so the backend is swappable.

## Package `tools/internal/forge`

```
Forge = Vcs + Runner + Flavor()
```

- **`Vcs`** — `SetSecret`, `SetVariable`, `SecretNames`, `Variables`,
  `CreateRepo`, `LockEnvironmentToBranch`, and a GitHub-shaped `API` escape hatch.
- **`Runner`** — `RunWorkflow`.
- **`Flavor()`** — `github` | `github-enterprise` | `gitlab`.

Backends:

| Backend | Type | CLI | Notes |
|---------|------|-----|-------|
| GitHub | `forge.GH` | `gh` | `NewGH(repo)` |
| GitHub Enterprise | `forge.GH` | `gh` | `NewGHEnterprise(host, repo)` — same CLI, drives `GH_HOST`, distinct flavor |
| GitLab | `forge.GL` | `glab` | `NewGitLab(host, repo)` — best-effort, see caveats |

`forge.Fake` is the in-memory test double (replaces the old `ghSetSecretFn` /
`ghSetRepoSecretFn` package-var seams).

## Selection (runtime)

`cmd/llz/forge.go` builds the backend from the environment:

- `LLZ_FORGE=gitlab` → GitLab (`glab`), host from `LLZ_GITLAB_HOST`.
- default → GitHub (`gh`); **GitHub Enterprise** when `LLZ_GH_HOST` is a
  non-`github.com` host.

Everything routes through the `forgeFn` / `forgeForFn` seams, so a single switch
reroutes the whole tool.

## Deliberately NOT abstracted

`selfupdate.go` downloads the **landing zone's own** `llz` releases from
github.com. That targets the template's releases, not the instance's forge, so it
stays a concrete `gh` call.

## GitLab caveats (best-effort, untested against a live instance)

This repo is GitHub-centric, so the GitLab backend maps concepts as closely as
the model allows but has not been validated end-to-end:

- No secret/variable split → `SetSecret` writes a **masked** CI/CD variable,
  `SetVariable` a plain one; reads filter on the `masked` flag.
- No per-file `workflow_dispatch` → `RunWorkflow` triggers a pipeline and passes
  the workflow name + inputs as pipeline variables.
- No deployment-branch-policies → `LockEnvironmentToBranch` maps onto protected
  branches + protected environments.
- The GitHub-shaped `API` escape hatch (used only by the GitHub-Actions
  attestation scan) returns `ErrUnsupported`.
- `CreateRepo` creates the project but does not yet push the working tree.

## Why not Bitbucket

Bitbucket was evaluated as a fourth flavor and deliberately **not** implemented.
The blocker is structural, not effort: the `Forge` abstraction models a forge
that is *both* the source host *and* the CI/secrets plane (the `Runner` surface +
secret/variable scopes), and Bitbucket does not fit that shape.

- **Bitbucket Server / Data Center** (the self-managed edition enterprises
  actually run) has **no forge-native CI**. Pipelines run on an *external* system
  — Jenkins / CloudBees / Bamboo — wired via webhooks ([Atlassian's CI/CD
  integration guide](https://confluence.atlassian.com/bitbucketserver094/integrate-your-ci-cd-pipeline-1489803073.html)).
  So the `Runner` surface (`RunWorkflow`) and the CI secret/variable surfaces
  (`SetSecret`, `SetVariable`, `SecretNames`, `Variables`) have no home on the
  forge: those credentials live in the external CI's store (e.g. Jenkins
  credentials), and builds are triggered there, not via a forge-side dispatch. A
  backend could only return `ErrUnsupported` for the majority of the interface —
  it would be a forge in name but serve almost none of what `llz` needs.
- **No CI templates to gate.** The `forge_flavor` mechanism's payoff is selecting
  rendered CI (the GHE workspace-perms workarounds; in principle GitLab `.gitlab-ci.yml`).
  A Bitbucket instance's CI is a Jenkins/CloudBees pipeline that this template
  does not (and would be a large, separate effort to) generate, so the flavor
  would gate nothing.
- **No first-class CLI.** GitHub has `gh` and GitLab has `glab`; Bitbucket Server
  has no equivalent, so even the source-only operations would mean hand-rolling
  REST-over-`curl`, diverging from the established backend pattern.
- **Even Bitbucket Cloud lacks a native package/container registry** (unlike
  GHCR/GitLab), so it is not a drop-in for the GitHub-shaped model either.

If a Bitbucket adopter appears, the realistic path is not a `forge.BB` backend but
a **separate CI integration** (e.g. a Jenkins `Runner`) plus a Jenkins/CloudBees
pipeline template — tracked as future work, not modeled here. Until then the
`Flavor` enum and `forge_flavor` choices stop at `github` / `github-enterprise` /
`gitlab`.

## GitHub Enterprise: flavor-gated CI templates

The **forge client** for GHE is just `forge.GH` + a host — `gh` behaves
identically. The real GHE-specific work is in the **rendered workflow/action
files**, ported from the `ohttp-bits` proto
(`bits.linode.com:functions/ohttp`, branch `main`).

### Scaffold-time flavor

`copier.yml` asks `forge_flavor` (`github` | `github-enterprise` | `gitlab`,
default `github`), mirroring `forge.Flavor()`. It is recorded in
`.copier-answers.yml`, so `copier update` carries it forward.

### Gating mechanism: input threading

Instances are thin callers of shared reusable workflows
(`<org>/lke-landing-zone/.github/workflows/llz-*.yml`) that dual-checkout the
template and source composite actions from it. So GHE workarounds can't be
selected purely at scaffold time — they're gated at run time by threading the
flavor as a workflow input:

1. **Every** thin caller forwards `forge_flavor: <@ forge_flavor @>` to its
   reusable workflow — `terraform.yml`, `promote.yml` (whose every generated
   stage forwards it, preserved by `tools/cmd/llz/promote_gen.go`), plus
   `bootstrap-dns`, `bootstrap-openbao`, `cluster-health`, `openbao-auto-unseal`,
   `scheduled-checks`, and `secret-rotation`.
2. **Every** reusable workflow (`llz-*.yml`, except the github.com-only
   `llz-release.yml`) declares a `forge_flavor` `workflow_call` input and, as the
   first step of every checkout-bearing job, runs the workspace-perms fix gated on
   `inputs.forge_flavor == 'github-enterprise'`. Workflows that call
   `llz-discover-deployments.yml` forward the flavor down to it. The flavor input
   default is `github`, so github.com instances are unaffected.

Runner selection is centralized off the same need: GHE has no github.com-hosted
runners, so every job's `runs-on` is `${{ vars.LLZ_RUNNER || 'ubuntu-latest' }}`.
A GHE (or any self-hosted) instance sets the `LLZ_RUNNER` repo/org variable once
to its runner label; github.com instances leave it unset and get `ubuntu-latest`.

The fix is inlined per job rather than a local composite action because it must
run *before* any checkout (the template — and thus its composite actions — isn't
on disk yet). The ported composite actions cover single-repo / persistent-runner
flows:

- `instance-template/.github/actions/fix-workspace-perms` — the `sudo chown -R`
  primitive (no-op-safe off GHE).
- `instance-template/.github/actions/checkout` — flavor-aware wrapper
  (fix-perms when GHE, then `actions/checkout`). `forge-flavor` input gates it.

Both live under `.github/actions/` (template-internal, excluded from instances,
classified `managed` in `.template-manifest`).

### Done / remaining (follow-ups)

Done: all reusable workflows + thin callers thread `forge_flavor`, every
checkout-bearing job runs the gated perms-fix, and `runs-on` is centralized on
`vars.LLZ_RUNNER`. Remaining:

- **CI forge selection for `llz`.** Export `LLZ_GH_HOST` (and/or `LLZ_FORGE`) in
  the reusable workflows when `forge_flavor` is GHE so the in-CI `llz` picks the
  GHE backend. Source the host from a repo variable.
- **`PATH` via `GITHUB_ENV`** (the ohttp-bits container-runner workaround) — only
  if a GHE adopter hits the containerized-`GITHUB_PATH` bug.

## GitLab: a parallel CI suite

GitHub Actions workflows cannot run on GitLab — GitLab executes `.gitlab-ci.yml`,
not `workflow_call` workflows. So `forge_flavor=gitlab` is not a gated *variant*
of the github.com workflows (the way GHE is); it selects a **separate CI
implementation** that mirrors the same topology and drives the same `llz` CLI.

| Layer | github.com / GHE | GitLab |
|---|---|---|
| Reusable definitions (template-hosted) | `.github/workflows/llz-*.yml` | `gitlab-ci/*.yml` |
| Instance entry point | `.github/workflows/*.yml` thin callers (`uses:@<ref>`) | `.gitlab-ci.yml` (`include: project:…, ref: <ref>`) |
| Reuse mechanism | `workflow_call` | `include:` + `extends:` + `!reference` |
| Shared glue | composite actions (`.github/actions/*`) | one base file (`gitlab-ci/base.yml`) |
| Runner selection | `runs-on: ${{ vars.LLZ_RUNNER }}` | `tags: [$LLZ_RUNNER]` |
| Approval / soak gate | GitHub Environment protection | protected GitLab environment + `when: manual` |
| Per-env secrets | environment secrets (`secrets: inherit`) | environment-scoped CI/CD variables |

**Centralization.** The operational logic is the *same `llz ci …` binary* on both
forges (baked into the ci-terraform image, which also ships `glab`). The only
GitLab-specific glue — git auth for `git::` module fetches and the `terraform
init` backend — lives **once** in `gitlab-ci/base.yml` as the `.llz-base` /
`.llz-git-auth` / `.llz-tf-init` anchors that every pipeline `extends`/`!reference`s,
the GitLab analog of the composite actions.

**Status: best-effort, not validated against a live GitLab instance** — the same
bar as the GitLab forge backend. Known gaps, documented in the files:

- **Per-region fan-out.** The github.com side discovers deployments and matrixes
  over them; GitLab can't matrix dynamically, so day-2 jobs drive on a single
  `$REGION` and loop `llz env list --json` when `$REGION` is empty/`all`. True
  parallel fan-out would use dynamic child pipelines.
- **Promotion is not generated yet.** `promote.yml` ships the reusable
  `.llz-promote-stage` block + a worked example; teaching `llz env pipeline` to
  emit a ranked GitLab stage chain (as `promote_gen.go` does for github.com) is
  the tracked follow-up.
- **Cross-host includes.** GitLab `include: project:` resolves within one GitLab
  instance, so a GitLab adopter must consume the template from a GitLab-hosted
  copy of `lke-landing-zone`, not from github.com.
