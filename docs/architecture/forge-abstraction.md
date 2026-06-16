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

1. The thin caller forwards `forge_flavor: <@ forge_flavor @>` to the reusable
   workflow (see `instance-template/.github/workflows/terraform.yml`, and the
   code-promotion caller `promote.yml`, whose every stage forwards it too — the
   generator `tools/cmd/llz/promote_gen.go` preserves it alongside the pin).
2. The reusable workflow declares a `forge_flavor` `workflow_call` input and, as
   the first step of every job (before checkout), runs the workspace-perms fix
   gated on `inputs.forge_flavor == 'github-enterprise'` (see
   `.github/workflows/llz-terraform.yml` — the reference conversion).

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

### Remaining work (follow-ups)

- **Convert the other reusable workflows** the same way `llz-terraform.yml` was:
  `llz-bootstrap-dns`, `llz-bootstrap-openbao`, `llz-cluster-health`,
  `llz-openbao-auto-unseal`, `llz-scheduled-checks`, `llz-secret-rotation`
  (declare the `forge_flavor` input, forward it from each thin caller, add the
  gated step). Mirror in each `instance-template/.github/workflows/*.yml` caller.
- **`runs-on` labels.** GHE has no github.com-hosted runners; a GHE instance must
  target self-hosted runner labels. Make `runs-on` flavor-aware (input or repo
  variable) — not yet done.
- **CI forge selection for `llz`.** Export `LLZ_GH_HOST` (and/or
  `LLZ_FORGE`) in the reusable workflows when `forge_flavor` is GHE so the in-CI
  `llz` picks the GHE backend. Source the host from a repo variable.
- **`PATH` via `GITHUB_ENV`** (the ohttp-bits container-runner workaround) — only
  if a GHE adopter hits the containerized-`GITHUB_PATH` bug.
