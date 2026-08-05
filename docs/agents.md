# Agent / Assistant Conventions

This repo supports contributors using **Claude Code**, **OpenAI Codex CLI**,
**Gemini CLI**, and any other tool that follows the
[`AGENTS.md` convention](https://agentsmd.net/). These tools discover `AGENTS.md`
directly, so no per-tool stub files are maintained at the repo root.

## Single source of truth

[`AGENTS.md`](../AGENTS.md) at the repo root is **canonical**. Only edit
`AGENTS.md` (and this file) when project-wide rules change — do not reintroduce
`CLAUDE.md` / `GEMINI.md` stubs.

## Nested scope

Several directories ship their own `AGENTS.md` that **overrides** the root where
rules conflict:

| Directory | Scope | Notes |
|-----------|-------|-------|
| `tools/` | Native Go host tooling | Single Go module; stdlib-first (only `sigs.k8s.io/yaml`) |
| `template-scripts/` | Shell + Python utilities | Bash conventions, destructive-op guards |
| `.github/workflows/` | CI/CD workflows | SHA-pinning + workflow conventions |
| `terraform-modules/` | Reusable Terraform modules | Publishing/versioning discipline (see RELEASING.md) |
| `instance-template/terraform-iac-bootstrap/` | Instance Terraform roots | Module-consumption + tfvars conventions |

## First-time setup

```bash
# Enable the shared git hooks (pre-commit secret scan, pre-push build + lint check)
git config core.hooksPath template-scripts/hooks
```

## Claude Code project automation (`.claude/`, `.mcp.json`)

Claude Code additionally loads project-scoped automation from this repo. These
are conveniences layered on top of the make/CI gates — the gates stay
authoritative; nothing here replaces `make lint`.

- **`.claude/settings.json`** — a permission allowlist for the read-only /
  lint commands the workflow uses, plus two hooks: a PreToolUse guard that
  blocks writing key material (`*.pem`/`*.der`/`*.key`, mirroring the
  pre-commit hook at edit time) and a PostToolUse formatter that runs
  `gofmt -w` / `tofu fmt` on edited files (mirroring `make fmt` / `make
  tf-fmt`). Scripts live in `.claude/hooks/`.
- **`.claude/skills/`** — procedure for the loops that cost the most rounds.
  Each one **routes**: it cites the canonical file rather than restating it, so a
  rule that changes in one place cannot rot in two. Grouped by when they fire:

  | Before you write code | While you work | Before it ships | Operating |
  |---|---|---|---|
  | `gate` — which gate proves this, and at which layer | `netpol-change` — the Cilium/LKE-E traps | `preflight` — which gates your diff triggers | `e2e-triage` — a red lane to a root cause |
  | `branch-base` — what to branch from, what a rebase orphans | `credential-change` — the fan-out of touching a secret | `delivered-surface` — what an adopter carries | `rotate-credentials` — run a rotation (user-invoked) |
  | `add-ci-guard` — the mechanics of a static guard | `docs` — auditing and writing this repo's Markdown | `release` — the umbrella tag (user-invoked) | `onboard-adopter` — coach the quickstart |

  `release` and `rotate-credentials` set `disable-model-invocation: true` — both
  are cloud-mutating or irreversible, so they are user-invoked only.

- **`.claude/agents/template-hygiene-reviewer.md`** — a read-only reviewer for
  the AGENTS.md conventions CI can't machine-check (org-identity hardcoding,
  prefix rules, scars-as-defaults comments).
- **`.mcp.json`** — the HashiCorp Terraform MCP server (registry/provider doc
  lookup; runs via Docker, no credentials). **Pinned by digest**, not by tag or
  `:latest`, for the reason `.github/workflows/AGENTS.md` gives for SHA-pinning
  every `uses:` — a mutable reference is a supply-chain hole regardless of which
  registry it points at. Bump the version and the digest together.

Other agent CLIs ignore these directories; `AGENTS.md` remains the canonical
cross-tool guidance.

## When these rules change

Edit only [`AGENTS.md`](../AGENTS.md) and this file. Keep the directory-level
`AGENTS.md` files in sync with the structure they describe.
