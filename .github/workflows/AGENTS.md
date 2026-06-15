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
- uses: docker/login-action@650006c6eb7dba73a995cc03b0b2d7f5ca915bee # v4.2.0
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

## Composite actions available

Prefer these over reimplementing their logic inline:

| Action | Purpose |
|--------|---------|
| `ghcr.io/<owner>/ci-terraform` | CI image with terraform, tflint, helm, kubectl, kustomize, checkov (bundles the `firewall-cidrs` Go binary) |

## Tool installation pattern

On GitHub-hosted runners, install CLI tools with the official marketplace setup actions (SHA-pinned, with an explicit pinned version) rather than hand-rolled `curl | tar` steps (a legacy `$HOME/.local/bin` workaround). The setup actions cache the tool and put it on `PATH`:

| Tool | Action |
|------|--------|
| helm | `azure/setup-helm` |
| kubectl | `azure/setup-kubectl` |
| yq | `dcarbone/install-yq-action` |
| kind (+ cluster) | `helm/kind-action` |

Pin the version from the workflow `env:` block, e.g. `version: v${{ env.HELM_VERSION }}`, so the tool version stays reproducible. Tools consumed inside the `ci-terraform` / `ci-kubernetes` container images are already baked into those images — don't re-install them. In particular `ci-terraform` ships `gh` and the prebuilt Go CLIs (`llz`, `firewall-cidrs`) on `PATH`, so `TF_IMAGE` jobs call them directly with no `setup-go`/`go build`/`install-*` step. They track the image tag (`vars.TF_IMAGE`), so keep `TF_IMAGE` in step with the template release the instance pins.

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

Every job must have an explicit `permissions:` block. The safe default for non-publishing jobs is:

```yaml
permissions:
  contents: read
```

Jobs that push to GHCR need `packages: write` (pulling a private GHCR `container:` image needs `packages: read`). Jobs that comment on PRs need `pull-requests: write`. Never omit the block — an absent `permissions:` inherits workflow-level defaults or write-all.

> Per-environment operational workflows (terraform apply, bootstrap, secret
> rotation, app deploy) are NOT shipped here — they live with the instance
> scaffolding under [../../instance-template/.github/workflows/](../../instance-template/.github/workflows/).
> The workflows in this directory build and publish the reusable artifacts: the
> Helm charts (`publish-charts.yml`), the CI tool images (`build-images.yml`),
> and the firewall-controller operator image (`firewall-controller.yml`), plus
> chart/manifest validation (`kubernetes.yml`).

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
| `firewall-controller.yml` | pushes the operator image tagged `:vX.Y.Z` |

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
