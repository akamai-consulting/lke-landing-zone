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
- **New behavior ships with a gate that fails when the behavior stops working.**
  Not a test that the code renders, parses, or is present — a test that the thing
  it does still happens. Two live regressions came from skipping this, and both
  passed every existing check: OpenBao's audit log shipped to a Service that never
  existed (the push URL and its NetworkPolicy named the same wrong namespace, so
  they agreed with each other and with nothing in the cluster), and volume
  relabeling renamed Volumes out from under the reaper's `pvc-` prefix (one
  commit, both sides of a contract, one side updated). Read
  [docs/e2e-gates.md](docs/e2e-gates.md) **before** adding a behavior — it has the
  two archetypes, the fail-closed doctrine, and how to wire a lane. Three rules
  carry most of it: assert at the CONSUMER on data the producer really emitted;
  call both sides' REAL functions rather than restating a shared rule; and fail
  closed on vacuity, because a gate that passes having examined nothing looks
  exactly like the outage it exists to catch.
- **NEVER attribute commits to Claude or any AI agent.** Do not add a
  `Co-Authored-By:` trailer (or any model/agent attribution) to commit messages,
  and do not set the git author or committer to an agent identity — commits carry
  the human contributor's name and email only.
- **Do NOT make code or config changes without explicit approval.**

## Repo layout

```
terraform-modules/   Reusable TF modules; published as git:: tagged sources (see RELEASING.md)
kubernetes-charts/   First-party Helm charts; published to GHCR as OCI artifacts
tools/               Native Go module: llz (adopter CLI + CI plumbing). firewall-cidrs/firewall-controller moved to the private lke-landing-zone-internal repo
dockerfiles/         Container images (ci-tofu, ci-kubernetes, devcontainer) → ghcr.io/akamai-consulting/*
template-scripts/    stamp/drift scaffold provenance, git hooks, ci helpers
instance-template/   Genericized starter material a downstream instance repo instantiates
docs/                adopter-guide.md, agents.md
.github/workflows/   Template CI: build/publish, lint + the gate suite, budget ratchets, release e2e, security scans. Conventions in its own AGENTS.md
```

Per-directory details live in each directory's `AGENTS.md`.

## Publishing & versioning discipline

This is the contract that makes the artifacts safely reusable — treat it as
load-bearing.

### One umbrella tag (`terraform-modules/RELEASING.md`)

- The whole landing zone versions in **lockstep under one bare SemVer tag
  `vX.Y.Z`**: the Terraform modules (`git::?ref=vX.Y.Z`), the reusable workflows +
  scaffold (the bodies are referenced by repo-local `./` paths per ADR 0003, so
  there is no cross-repo `uses:@` to pin, and no `template-ref:` input either —
  CI reads the pin from `.copier-answers.yml` at runtime), and the `llz` CLI
  binaries — all at the same commit. (Helm charts are the
  exception: independently versioned via `Chart.yaml`, see below.)
- **The delivered scaffold carries no version at all.** `instance-template/` and
  the delivered docs contain zero `<@ llz_version @>` tokens: the pin lives once in
  `.copier-answers.yml`, and everything reads it at runtime (`pinnedTemplateRef()`),
  so a content-free version bump changes exactly that one file. The single
  deliberate exception — the pinned docs pointer — is *generated* by `llz ci
  deliver-docs`, not stored. `llz lint`'s upgrade-churn guard enforces this: adding a
  version reference to the delivered surface fails the gate, because it would add a
  line to every instance's upgrade diff on every release forever. Restating the pin
  in prose counts too — link to `.copier-answers.yml` instead. The guard runs on
  both sides of the delivery: in an instance checkout it scans the vendored
  workflows and kept docs, so a stale string that an older `llz` actually delivered
  is caught there too rather than sitting unnoticed release after release.
- **A release is two human steps, gated by e2e.** (1) Publish a **pre-release**
  `vX.Y.Z` → fires `release: prereleased` → `release-e2e.yml` stands up a real
  cluster. The pre-release tag is ignored by `llz self-update`/`new`, and no
  binaries/image are built yet. (2) Once e2e is green, **promote** it to a full
  release (uncheck pre-release) → fires `release: released` → `llz-release.yml`
  (binaries). The promote click is the
  approval; nothing public exists until it. There is nothing to bump first — the
  template hardcodes no version.
- **Tags are immutable** — never move a tag. To release a change, cut a new one.
- **SemVer on the interface:** MAJOR = breaking module-IO / reusable-workflow-input
  / scaffold change, MINOR = backward-compatible addition, PATCH = fix. The module
  READMEs and the reusable workflows' `on.workflow_call` are the SemVer surface.
- Internal module-to-module references stay **relative** (`../llz-<name>`), never
  `git::` — that keeps the two halves pinned to the same umbrella tag. (There are
  none today; each root composes the modules directly.)
- **The first-party pins live in the embedded TF roots, not in the scaffold.**
  `instance-template/` holds no `<@ llz_version @>` token at all — as the bullet
  above says, and as `terraform-iac-bootstrap/` shows: it ships a `.gitignore`, an
  `AGENTS.md` and two provider lockfiles, and not one `*.tf`. The roots that carry
  the `git::…?ref=<@ llz_version @>` module sources are embedded in the `llz`
  binary (`tools/internal/shared/tfroots/roots/`). `llz render` materialises them
  into **gitignored** `*.tf`, substituting `<@ upstream_org @>` and
  `<@ llz_version @>` at that moment from the pin `resolveTemplateRef()` reads —
  which `llz new`/`llz upgrade` set to the CLI's own version, the version anchor.
  So the pin is stamped at render time and never committed, which is *why* the
  no-version-in-the-scaffold rule above is satisfiable at all. Don't write a
  literal version into those roots, and don't add a bump step — Renovate is
  disabled on the first-party self-references so `llz upgrade` stays the single
  channel.

### Helm charts (`kubernetes-charts/README.md`)

- Charts publish to GHCR as OCI: `oci://ghcr.io/akamai-consulting/charts/<chart>:<version>`.
- **Immutable by convention:** `publish-charts.yml` skips any chart whose
  `Chart.yaml` `version:` is already published. To release, bump `version:` —
  never overwrite an existing tag (Argo Applications pin `targetRevision: X.Y.Z`).
- `helm lint --strict` + `helm template` must be clean for every chart.

### Container images (`.github/workflows/build-images.yml`)

- `ci-tofu`, `ci-kubernetes`, `devcontainer` build multi-arch (amd64 +
  arm64) to `ghcr.io/akamai-consulting/*`. `ci-tofu` builds the `llz` Go
  binary from the `tools/` module (supplied via
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
4. `make test-race` — CI runs it and **nothing else in this list does**. `go test
   ./...`, `make lint`, `staticcheck`, `coverage` and `core-surface-check` all
   pass over a data race. One in `assertsuite` failed this deterministically
   while every other gate was green, and survived five pushes because nobody ran
   the target.
5. **`make lint` — the authoritative final gate; fix every issue until it exits
   0.** It is change-aware (keys off `git diff HEAD`) and covers everything you
   touched: Go (`gofmt`/`go vet`), `shellcheck`, Terraform (`tofu fmt`,
   `tflint`, `checkov`), Kubernetes (`kube-linter`, `kubeconform`), Helm
   (`helm lint --strict`), and `actionlint` for `.github/workflows/*.yml`.
   (`make LINT_ALL=1 lint` runs every check unconditionally.)
6. **If you changed behavior, name the gate that would catch it regressing** — in
   the PR body, in one line. If the honest answer is "none", write the gate
   ([docs/e2e-gates.md](docs/e2e-gates.md)); if the behavior genuinely doesn't
   need one (refactor, docs, a statically-decidable invariant already in
   `make lint`), say which and why. A green `make lint` is not evidence a
   behavior works — both of the regressions this rule comes from were green.

## Where to look

| Topic | File |
|-------|------|
| End-to-end adopter path | [docs/adopter-guide.md](docs/adopter-guide.md) |
| Agent convention (this file's rules) | [docs/agents.md](docs/agents.md) |
| Non-obvious gotchas / hard-won lessons | [docs/lessons-learned.md](docs/lessons-learned.md) |
| Testing behavior — when a gate is required, and how to write one | [docs/e2e-gates.md](docs/e2e-gates.md) |
| Terraform module release contract | [terraform-modules/RELEASING.md](terraform-modules/RELEASING.md) |
| Helm chart inventory + OCI publishing | [kubernetes-charts/README.md](kubernetes-charts/README.md) |
| Contributor workflow, prereqs, git hooks | [CONTRIBUTING.md](CONTRIBUTING.md) |
