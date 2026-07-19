# LKE Landing Zone (LLZ) — Agent & Contributor Conventions

Canonical instructions for AI agents and contributors working in **this template
repo**. LLZ is a reusable, secure-by-default LKE-Enterprise landing zone: it
builds and publishes versioned Terraform modules, Helm charts, CI images, and a
scaffold generator that a downstream instance repo consumes.

> **Canonical agent guidance.** This file is discovered directly by Claude Code,
> Codex CLI, Gemini CLI, and (via `.github/copilot-instructions.md`) GitHub
> Copilot. Edit this file only — do not duplicate its content into stubs. See
> [docs/agents.md](docs/agents.md) for the full convention.

> **Nested scope.** Top-level directories carry their own `AGENTS.md` that
> **overrides** this file where rules conflict (e.g. `tools/` is a Go module;
> `template-scripts/` adds Bash + destructive-op guards). Read the directory's
> `AGENTS.md` before editing inside it.

> **Lessons learned.** Before non-trivial work, skim
> [docs/lessons-learned.md](docs/lessons-learned.md) for non-obvious gotchas
> (repo topology, LKE-Enterprise constraints, CI runner migration, placeholders)
> that aren't derivable from the code alone.

## Critical constraints

- **This repo ships reusable artifacts, not a running deployment.** The cluster,
  the per-env tfvars, and the live apl-values overlays belong to a *downstream
  instance repo*. Here you build and publish modules/charts/images and maintain
  the scaffolding.
- **No org-identity hardcoding.** Linode + apl-core assumptions stay as
  **defaults**; only org/cluster identity — endpoints, domains, CIDRs, names,
  registry/repo URLs — is variabilized. Never bake a specific org's host, GHCR
  org, GitOps repo URL, or domain into a module or chart. Instance-specific
  literals live in `instance-template/` (and are filled in by the instance repo),
  not in the reusable trees.
- **No org/`platform-` prefix** on Terraform resource names, Helm resource names,
  or release names — names stay generic so two system teams don't collide.
- **Scars as defaults.** Every non-obvious value ships as a default with a comment
  explaining the failure mode it prevents.
- **NEVER attribute commits to Claude or any AI agent.** Do not add a
  `Co-Authored-By:` trailer (or any model/agent attribution) to commit messages,
  and do not set the git author or committer to an agent identity — commits carry
  the human contributor's name and email only.
- **Do NOT make code or config changes without explicit approval.**

## Repo layout

```
terraform-modules/   Reusable TF modules; published as git:: tagged sources (see RELEASING.md)
kubernetes-charts/   First-party Helm charts; published to GHCR as OCI artifacts
tools/               Native Go module: llz (adopter CLI + CI plumbing), firewall-cidrs, firewall-controller
dockerfiles/         Container images (ci-terraform, ci-kubernetes, devcontainer) → ghcr.io/akamai-consulting/*
template-scripts/    stamp/drift scaffold provenance, git hooks, ci helpers
instance-template/   Genericized starter material a downstream instance repo instantiates
docs/                adopter-guide.md, agents.md
.github/workflows/   build-images.yml, publish-charts.yml, ci-gate.yml, kubernetes.yml
```

Per-directory details live in each directory's `AGENTS.md`.

## Publishing & versioning discipline

This is the contract that makes the artifacts safely reusable — treat it as
load-bearing.

### One umbrella tag (`terraform-modules/RELEASING.md`)

- The whole landing zone versions in **lockstep under one bare SemVer tag
  `vX.Y.Z`**: the Terraform modules (`git::?ref=vX.Y.Z`), the reusable workflows +
  scaffold (`uses:@vX.Y.Z` / `template-ref:`), the `llz` CLI binaries, and the
  `firewall-controller` image — all at the same commit. (Helm charts are the
  exception: independently versioned via `Chart.yaml`, see below.)
- **A release is two human steps, gated by e2e.** (1) Publish a **pre-release**
  `vX.Y.Z` → fires `release: prereleased` → `release-e2e.yml` stands up a real
  cluster. The pre-release tag is ignored by `llz self-update`/`new`, and no
  binaries/image are built yet. (2) Once e2e is green, **promote** it to a full
  release (uncheck pre-release) → fires `release: released` → `llz-release.yml`
  (binaries) + `firewall-controller.yml` (image). The promote click is the
  approval; nothing public exists until it. There is nothing to bump first — the
  template hardcodes no version.
- **Tags are immutable** — never move a tag. To release a change, cut a new one.
- **SemVer on the interface:** MAJOR = breaking module-IO / reusable-workflow-input
  / scaffold change, MINOR = backward-compatible addition, PATCH = fix. The module
  READMEs and the reusable workflows' `on.workflow_call` are the SemVer surface.
- Internal module-to-module references stay **relative** (`../llz-<name>`), never
  `git::` — that keeps the two halves pinned to the same umbrella tag. (There are
  none today; each root composes the modules directly.)
- **The template hardcodes no version.** `instance-template/`'s first-party pins
  are copier `<@ llz_version @>` placeholders; `llz new`/`llz upgrade` render them
  to the `llz` binary's own version (the CLI is the version anchor). Don't write a
  literal version into those pins, and don't add a bump step — Renovate is disabled
  on them so `llz upgrade` stays the single channel.

### Helm charts (`kubernetes-charts/README.md`)

- Charts publish to GHCR as OCI: `oci://ghcr.io/akamai-consulting/charts/<chart>:<version>`.
- **Immutable by convention:** `publish-charts.yml` skips any chart whose
  `Chart.yaml` `version:` is already published. To release, bump `version:` —
  never overwrite an existing tag (Argo Applications pin `targetRevision: X.Y.Z`).
- `helm lint --strict` + `helm template` must be clean for every chart.

### Container images (`.github/workflows/build-images.yml`)

- `ci-terraform`, `ci-kubernetes`, `devcontainer` build multi-arch (amd64 +
  arm64) to `ghcr.io/akamai-consulting/*`. `ci-terraform` builds the
  `firewall-cidrs` Go binary from the `tools/` module (supplied via
  `--build-context tools-src=tools`) in a multi-stage build.
- `devcontainer` is the adopter-workstation image consumed by an instance's
  `.devcontainer/devcontainer.json`; keep its tool versions in lockstep with the
  CI images so local checks match CI. See `docs/devcontainer.md`.

## Where instance-specific things live

`instance-template/` is the only place that holds environment- or org-shaped
material — Terraform roots, an example `apl-values` env, and instance workflows +
composite actions, all with placeholders. The reusable trees
(`terraform-modules/`, `kubernetes-charts/`, `tools/`) must stay org-agnostic.
`llz env add` is the generator that stamps a new environment from
this starter material into a downstream instance repo.

## Before submitting

The git hooks in `template-scripts/hooks/` enforce this at commit/push time (wire them via
`git config core.hooksPath template-scripts/hooks`), but run it yourself first:

1. `gofmt -w .` in `tools/`; `tofu fmt` any `.tf` files you changed.
2. `go vet ./...` in `tools/`.
3. `go test ./...` in `tools/` for any code you touched.
4. **`make lint` — the authoritative final gate; fix every issue until it exits
   0.** It is change-aware (keys off `git diff HEAD`) and covers everything you
   touched: Go (`gofmt`/`go vet`), `shellcheck`, Terraform (`tofu fmt`,
   `tflint`, `checkov`), Kubernetes (`kube-linter`, `kubeconform`), Helm
   (`helm lint --strict`), and `actionlint` for `.github/workflows/*.yml`.
   (`make LINT_ALL=1 lint` runs every check unconditionally.)

## Where to look

| Topic | File |
|-------|------|
| End-to-end adopter path | [docs/adopter-guide.md](docs/adopter-guide.md) |
| Agent convention (this file's rules) | [docs/agents.md](docs/agents.md) |
| Non-obvious gotchas / hard-won lessons | [docs/lessons-learned.md](docs/lessons-learned.md) |
| Terraform module release contract | [terraform-modules/RELEASING.md](terraform-modules/RELEASING.md) |
| Helm chart inventory + OCI publishing | [kubernetes-charts/README.md](kubernetes-charts/README.md) |
| Contributor workflow, prereqs, git hooks | [CONTRIBUTING.md](CONTRIBUTING.md) |
