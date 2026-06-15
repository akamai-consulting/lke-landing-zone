# Contributing

This repo is the **LKE Landing Zone (LLZ)** template: it builds and publishes the
reusable Terraform modules, Helm charts, and CI images that downstream instance
repos consume. Most contributions touch `terraform-modules/`, `kubernetes-charts/`,
`tools/`, or `instance-template/`.

## Prerequisites

- [Go](https://go.dev/dl/) toolchain (1.23+) — for the `tools/` module
- `terraform`/`tofu`, `kubectl`, `helm` — for the modules and charts

`llz doctor` is the authoritative, always-current list of the runtime toolchain
(and reports which are installed); the Go toolchain above is additionally needed
to build `llz` itself from source.

## Repo layout

See [README.md → What it ships](README.md#what-it-ships) for the canonical layout
and the published-artifact contract.

## Git hooks

The repo ships hooks in `template-scripts/hooks/`. Enable once per clone:

```bash
git config core.hooksPath template-scripts/hooks
```

- **pre-commit** — blocks committing secret files (`*.pem`, `*.der`, `*.key`).
- **pre-push** — builds the `tools/` Go module (when it changed) and runs the
  lint gate before the push lands.

## Build

Native tools (single Go module at `tools/`):

```bash
cd tools && go build ./...
```

The commands are `llz`, `firewall-cidrs`, and `firewall-controller`. There is
no other Go module in this repo.

## Lint

`make lint` is the authoritative gate. It is change-aware (keys off `git diff
HEAD`) and covers everything you touched — Go (`gofmt`/`go vet`), `shellcheck`,
Terraform, Kubernetes manifests, Helm charts, and GitHub workflows. Run the
auto-formatters first (`gofmt -w .` in `tools/`, `tofu fmt` for `.tf` files),
since `make lint` only verifies formatting, it does not rewrite it.

```bash
make lint                 # change-aware gate; fix until it exits 0
make LINT_ALL=1 lint      # run every check unconditionally
```

When you touch the `llz` CLI (`tools/**`) or its functional harness, `make lint`
also runs `make llz-functional` offline (`LLZ_FUNCTIONAL_NET=0`) — building the
binary and asserting its basic commands behave. The networked install/self-update
flow (`gh release download`, the authenticated `curl`, `llz self-update`) is the
same harness with `LLZ_FUNCTIONAL_NET=1`; it runs against a real release in
`release-e2e.yml`, not on every commit.

## Scaffold a new deployment

To generate an environment for a downstream instance repo:

```bash
DRY_RUN=1 template-scripts/new-deployment.sh <env> --region us-sea --obj-cluster us-sea-1
template-scripts/new-deployment.sh <env> --region us-sea --obj-cluster us-sea-1
```

See [docs/adopter-guide.md](docs/adopter-guide.md) for the end-to-end path.

## AI assistant instructions

[`AGENTS.md`](AGENTS.md) is the canonical instruction file, discovered directly by
Claude Code, Codex CLI, Gemini CLI, and (via `.github/copilot-instructions.md`)
GitHub Copilot. Per-directory overrides live in each directory's `AGENTS.md`.

## Commit style

- No `Co-Authored-By` lines.
- No code or config changes without explicit approval.
- Conventional commit prefixes are welcome but not required.
