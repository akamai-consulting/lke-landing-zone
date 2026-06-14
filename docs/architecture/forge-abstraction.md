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

## GitHub Enterprise: the real work is in the templates (follow-up)

The **forge client** for GHE is just `forge.GH` + a host — `gh` behaves
identically. But a working GHE instance needs **different rendered workflow/action
files**, not a different Go client. The `ohttp-bits` proto
(`bits.linode.com:functions/ohttp`, branch `main`) shows the workarounds a
self-hosted-runner-on-GHE deployment requires:

- Local composite actions under `.github/actions/` (e.g. a `checkout` wrapper)
  instead of bare marketplace `uses:` — plus a `fix-workspace-perms` step before
  checkout (containerized self-hosted runners leave root-owned workspaces).
- `PATH` set via `GITHUB_ENV` rather than `GITHUB_PATH` (unreliable in
  containerized GHA jobs on some runner versions).

Porting these into the copier templates as a GHE flavor (so an instance scaffolds
the right workflow variants for its `Flavor()`) is a separate, larger workstream
tracked outside this change. The Go abstraction here is the prerequisite: `llz`
can already tell which forge it targets via `Flavor()`.
