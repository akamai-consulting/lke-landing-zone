# .github/workflows/ — CI/CD Workflows

> **Scope.** This directory contains GitHub Actions workflow files only; the root [../../AGENTS.md](../../AGENTS.md) conventions apply except where the workflow-specific guidance below overrides them.

## Runners

These workflows run on GitHub-hosted runners (`runs-on: ubuntu-latest`). Do not use `[self-hosted]` — the workspace is clean on every run, so the old `fix-workspace-perms` (sudo chown) and known_hosts workarounds are gone. Docker (with buildx + QEMU for multi-arch), `make`, `python3`, and `curl` are all pre-installed.

## GHCR authentication

Push and pull GHCR using the built-in `GITHUB_TOKEN` — never a personal PAT:

```yaml
permissions:
  contents: read
  packages: write   # write to push images/charts; read to pull a private CI image
```

```yaml
- uses: docker/login-action@af1e73f918a031802d376d3c8bbc3fe56130a9b0 # v4.4.0
  with:
    registry: ghcr.io
    username: ${{ github.actor }}
    password: ${{ secrets.GITHUB_TOKEN }}
```

Always derive the GHCR namespace from the repo owner, lowercased, so images/charts follow the repo into whatever org owns it (and an adopter's fork) — never a hardcoded account:

```bash
echo "repo=ghcr.io/${GITHUB_REPOSITORY_OWNER,,}" >> "$GITHUB_OUTPUT"
```

Jobs that use a private GHCR image as a `container:` need `packages: read` and these `container.credentials`:

```yaml
container:
  image: ${{ vars.KUBE_IMAGE }}
  credentials:
    username: ${{ github.actor }}
    password: ${{ secrets.GITHUB_TOKEN }}
```

A job's `container.image`, **when it names one of this repo's own CI images**
(`ci-tofu` / `ci-kubernetes`), uses **`:latest`, never a version** —
`format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner)` — and
`llz ci version-pins` fails the build if one restates the Dockerfile ARG instead.
It finds them by YAML position (`jobs.<id>.container.image`, and service
containers), not by matching the fallback expression, so the rule holds however
the value is spelled. Third-party images (`postgres:16`, say) are not its
business and keep their own pins. In `instance-template/` the rule inverts
again: a delivered workflow must resolve its image from `vars.TF_IMAGE` /
`vars.KUBE_IMAGE` and hardcode nothing, because an instance's image has to match
the template ref it is pinned to.
The fallback used to be version-pinned, which made a `KUBECTL_VERSION` /
`TOFU_VERSION` bump self-defeating: `build-images.yml` publishes on pushes to
`main` while Lint runs on the bump's own push, so every bump pointed Lint at a tag
that did not exist yet and cost one `manifest unknown` red plus a manual re-run.
The fallback fires whenever the repo variable is unset — which on this repo is
always, since neither `TF_IMAGE` nor `KUBE_IMAGE` is set here. It is the live
path for every Lint run, not a fresh-fork edge case, which is why the scar above
happened here. Where nothing pins the image a moving tag is the honest answer;
pin `TF_IMAGE`/`KUBE_IMAGE` to a `:sha-<commit>` tag if you need reproducibility
— a version tag is not it, since that moves too until the next bump freezes it.

## Composite actions available

Prefer these over reimplementing their logic inline:

| Action | Purpose |
|--------|---------|
| `./.github/actions/setup-llz` | Sets up Go (version from `tools/go.mod`) and builds the `llz` CLI onto `PATH`. The only composite action in this repo's own CI — use it instead of a hand-rolled `setup-go` + `go build` pair. `actions/setup-go` must appear nowhere else. (`instance-template/.github/actions/` ships seven more, but those are scaffold content an *instance* runs, not CI for this repo.) |
| `ghcr.io/<owner>/ci-tofu` | CI image with terraform, tflint, helm, kubectl, kustomize, checkov (bundles the `firewall-cidrs` Go binary) |

**This is enforced** — `make setup-go-sole-site` (in the `llz-gates` suite, so
every `make lint` runs it) fails on any `uses: actions/setup-go` outside the
composite. It was written because the rule above had already been broken:
`release-e2e-lane.yml` carried a second pin at **v7.0.0** while the composite sat
at **v6.5.0**, in a job running the same functional script `llz-release.yml` runs
*through* the composite. Nothing caught it, because actionlint judges each
`uses:` in isolation and a correctly SHA-pinned action looks identical whether or
not it is the right one.

The one deliberate exception is `llz-release.yml`'s **`go build`**, which it
hand-rolls to stamp the real release version via `-ldflags` — something the
composite intentionally does not do. Note the exception is the *build* only: that
job still takes its **toolchain** from `setup-llz`, which is why the sole-site
rule above holds with no exemptions at all.

## Tool installation pattern

On GitHub-hosted runners, install CLI tools with the official marketplace setup actions (SHA-pinned, with an explicit pinned version) rather than hand-rolled `curl | tar` steps (a legacy `$HOME/.local/bin` workaround). The setup actions cache the tool and put it on `PATH`:

| Tool | Action |
|------|--------|
| helm | `azure/setup-helm` |
| kubectl | `azure/setup-kubectl` |
| yq | `dcarbone/install-yq-action` |
| kind (+ cluster) | `helm/kind-action` |

Pin the version from the workflow `env:` block, e.g. `version: v${{ env.HELM_VERSION }}`, so the tool version stays reproducible. Tools consumed inside the `ci-tofu` / `ci-kubernetes` container images are already baked into those images — don't re-install them. In particular `ci-tofu` ships `gh` and the prebuilt Go CLIs (`llz`, `firewall-cidrs`) on `PATH`, so `TF_IMAGE` jobs call them directly with no `setup-go`/`go build`/`install-*` step.

**Carve-out — do not "clean up" `setup-llz` inside a container job.** A PR that
changes `tools/` runs BEFORE the image carrying that change is rebuilt, and the
images ship no Go toolchain, so the Makefile's `go run ./cmd/llz` fallback cannot
fire either. Two jobs in `lint.yml` therefore run `setup-llz` inside the container
on purpose and are load-bearing: the `ci-kubernetes` guard job, and the
`template-manifest` gate (`make lint-tf` → `template-manifest-check` → `go run
./cmd/llz ci template-manifest`). Each carries its own inline justification.
Once the rebuilt image catches up they are fast, redundant no-ops — not dead steps. They track the image tag (`vars.TF_IMAGE`), so keep `TF_IMAGE` in step with the template release the instance pins.

## CRD installation

Use `helm template --include-crds | yq 'select(.kind == "CustomResourceDefinition")' | kubectl apply -f -`, not raw GitHub release URLs. Release URLs break when assets are renamed between releases.

For ESO specifically, ESO places CRDs in `templates/` not `crds/`, so `--set crds.create=true` is required:

```bash
helm template eso external-secrets/external-secrets --version "$ESO_HELM_VERSION" \
  --set crds.create=true \
  | yq 'select(.kind == "CustomResourceDefinition")' \
  | kubectl apply -f -
```

For ArgoCD, argo-cd also places CRDs in `templates/`:

```bash
helm template argocd argo/argo-cd --version "$ARGOCD_HELM_VERSION" \
  --include-crds \
  | yq 'select(.kind == "CustomResourceDefinition")' \
  | kubectl apply -f -
```

## Git SSH host-key handling

Any job that performs git operations must set this at job or workflow env level:

```yaml
env:
  GIT_SSH_COMMAND: ssh -o StrictHostKeyChecking=accept-new
```

This avoids interactive host-key prompts hanging a job the first time it talks to a new git host.

## permissions blocks

**Every workflow must declare a workflow-level `permissions:` block, and the
default is the whole of it:**

```yaml
permissions:
  contents: read
```

That is the load-bearing rule, because a job with no block and a workflow with no
block is what inherits the repository default — up to write-all. With the
workflow-level block present, a job that omits its own inherits exactly
`contents: read`, which is the floor, not a risk.

**A job declares its own block when, and only when, it needs MORE than that** —
`packages: write` to push to GHCR, `packages: read` to pull a private GHCR
`container:` image, `pull-requests: write` to comment on a PR, `actions: write`
to trigger or inspect another workflow's runs, `contents: write` to publish a
release. Job-level permissions REPLACE the workflow-level set rather than adding
to it, so a job that declares one must list everything it needs — including
`contents: read` if it checks out. (`llz-release.yml`'s `image-tag` job is the
instructive counter-example: it declares `packages: write` and nothing else
because it never checks out, only retags an image.)

> This section used to read "Every job must have an explicit `permissions:`
> block … never omit the block", and 28 of the 82 jobs across this repo and
> `instance-template/` omitted it — correctly, because every one of their
> workflows declares `contents: read` at the top. A rule that most of the tree
> already violates safely is one people learn to skip past, and it hid the
> distinction that actually matters: the danger is a missing block at BOTH
> levels, not a missing block at the job level.

> Per-environment operational workflows (terraform apply, bootstrap, secret
> rotation, app deploy) are NOT shipped here — they live with the instance
> scaffolding under [../../instance-template/.github/workflows/](../../instance-template/.github/workflows/).
> The workflows in this directory build and publish the reusable artifacts: the
> Helm charts (`publish-charts.yml`) and the CI tool images (`build-images.yml`,
> a matrix over terraform / kubernetes / devcontainer / llz — the in-cluster
> reconciler and Harbor components ship in the `llz` image, so there is no
> separate operator-image workflow). Chart/manifest validation lives in
> `lint.yml`, which merged the former `kubernetes.yml` and `terraform.yml`.

## Release orchestration

A release goes public in **two human steps, gated by e2e** (see
[terraform-modules/RELEASING.md](../../terraform-modules/RELEASING.md)):

1. **Publish a pre-release `vX.Y.Z`** → `release: prereleased`:

| Workflow | On `prereleased` |
|----------|------------------|
| `release-e2e.yml` | real-cluster create → validate → destroy gate |

2. **Promote to a full release** (uncheck pre-release) once e2e is green →
   `release: released`:

| Workflow | On `released` |
|----------|---------------|
| `llz-release.yml` | builds + attaches the `llz` CLI binaries |

Keyed off release events (not a tag push) because a release created with the
built-in `GITHUB_TOKEN` suppresses downstream runs — the human publishing the
pre-release arms e2e, and promoting it (the approval click) arms the binaries/image.
A pre-release tag is ignored by `llz self-update`/`new`, so an un-promoted candidate
is never consumable. There is no pin-bump step: `instance-template/`'s first-party
pins are copier `<@ llz_version @>` placeholders that `llz new`/`llz upgrade` render
to the `llz` binary's own version — a fresh scaffold from tag `vX.Y.Z` references
`vX.Y.Z`, no chicken-and-egg.

## Rules that apply from root

- Never add `Co-Authored-By` to commits.
- Do not make workflow changes without explicit user approval.
- SHA-pin all `uses:` references. Tag references (`@v4`) are not acceptable — use the full commit SHA with a `# vN` comment.
