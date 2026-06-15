# Dev Container

> **Goal:** open your instance repo in a ready-made workstation that already has
> every CLI this flow needs — no host installs, no version drift from CI.
>
> **Audience:** anyone working in an LLZ instance repo who'd rather not hand-
> install `terraform`, `kubectl`, `helm`, `bao`, `copier`, `gh`, and the rest.

Your instance ships a [Dev Container](https://containers.dev) at
[`.devcontainer/devcontainer.json`](../instance-template/.devcontainer/devcontainer.json). It points
at a **prebuilt, multi-arch image** that the upstream template publishes —
`ghcr.io/<upstream-org>/devcontainer` — so opening the repo gives you the exact
toolchain the bootstrap scripts and CI use, on **amd64 and arm64** (native on
Intel/AMD and Apple Silicon).

It is an alternative to installing tooling on your laptop. The cluster bootstrap
still runs in GitHub Actions either way — the container is for the local loop:
editing tfvars and `apl-values/`, running `llz`, `copier update`, `kubectl`, and
the linters before you push.

---

## What's inside

The image bundles the same set [`llz doctor`](quickstart.md#confirm-readiness--llz-doctor)
checks, so a fresh container reports green tooling out of the box:

| Tool | For |
|---|---|
| `llz` | the adopter CLI that drives the whole flow — built from this template commit |
| `terraform` | the Terraform roots under `terraform/` |
| `kubectl`, `helm`, `kustomize` | rendering + inspecting the cluster |
| `bao` | the OpenBao CLI |
| `copier` | `llz upgrade` / `copier update` to pull template releases |
| `gh` | dispatching workflows, reading repo config |
| `linode-cli` | the `llz tokens` wizard + OBJ-cluster lookup |
| `jq`, `yq`, `ssh-keygen` | the bootstrap scripts' workhorses |
| `tflint`, `checkov` | Terraform lint + IaC security scan |
| `kube-linter`, `kubeconform` | Kubernetes manifest lint + schema validation |
| `actionlint`, `shellcheck` | workflow + script linting |
| `argo` | inspecting the cert-automation / OpenBao Argo workflows |

Tool versions track the `ci-terraform` / `ci-kubernetes` images, so what passes
locally passes in CI.

`llz` is **built into the image** from the same template commit, so it's on your
`PATH` the moment the container starts (`llz version`). To move to a *different*
release — e.g. a newer one published since the image was built, or one matching
an instance pinned to an older template — run `llz self-update` (optionally
`--ref v0.2.0`), or install over it with the one-liner from
[Quick Start §2](quickstart.md#2-install-llz).

---

## Open it

**Prerequisites:** a container runtime (Docker Desktop, Colima, Podman, …) and one of:

- **VS Code** with the [Dev Containers](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers)
  extension — open the repo, then **“Reopen in Container”** (Command Palette →
  *Dev Containers: Reopen in Container*). First open pulls the image; later opens
  are instant.
- **The [`devcontainer` CLI](https://github.com/devcontainers/cli)** — headless:
  ```bash
  devcontainer up --workspace-folder .
  devcontainer exec --workspace-folder . llz doctor --env <env>
  ```
- **GitHub Codespaces** — *Code → Codespaces → Create*, which reads the same file.

The image is a **public** GHCR package, so the pull needs no credentials.

---

## First-run setup inside the container

The container starts clean — wire up your identity once per rebuild:

```bash
gh auth login                       # authenticate gh (workflows, repo config)
llz doctor --env <env>              # confirm the toolchain + your repo config
```

**Forwarding host credentials (optional).** To avoid re-authenticating, VS Code
can share your host setup with the container:

- **Git / SSH:** VS Code forwards your host SSH agent automatically, and copies
  your `~/.gitconfig` in — so commits and the ArgoCD deploy-key SSH wiring work
  with your host keys.
- **`gh` login:** mount it in by adding to `.devcontainer/devcontainer.json`:
  ```jsonc
  "mounts": [
    "source=${localEnv:HOME}/.config/gh,target=/home/vscode/.config/gh,type=bind"
  ]
  ```

Treat anything you mount as present in the container — don't mount secrets you
don't want a container process to read.

---

## Forks: building your own image

If you forked the template and want the devcontainer to track **your** tools, the
image build ships with it:

1. The Dockerfile lives at
   [`dockerfiles/devcontainer/Dockerfile`](https://github.com/akamai-consulting/lke-landing-zone/blob/main/dockerfiles/devcontainer/Dockerfile)
   in the template repo; [`.github/workflows/build-images.yml`](https://github.com/akamai-consulting/lke-landing-zone/blob/main/.github/workflows/build-images.yml)
   builds and pushes it (amd64 + arm64) to `ghcr.io/<your-org>/devcontainer` on
   every change under `dockerfiles/**`. Trigger it manually with **Run workflow →
   image: devcontainer**.
2. Point your instance at it: `image:` in `.devcontainer/devcontainer.json` is
   rendered from your `upstream_org` Copier answer, so it already targets your
   org once you scaffold from your fork. (The file is a `merge` in
   `.template-manifest` — your org edit survives template updates.)

**Pinning.** `:latest` follows the weekly rebuild. For a reproducible workstation,
pin to an immutable digest tag — replace `:latest` with the `:sha-<commit>` tag
the build also pushes.

---

## See also

- [Quick Start](quickstart.md) — the end-to-end `llz` flow (install `llz` in §2)
- [Adopter guide §1](adopter-guide.md#1-prerequisites) — the CLI tooling the
  container satisfies
- [`llz doctor`](quickstart.md#confirm-readiness--llz-doctor) — verify the
  toolchain + repo config from inside the container
