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

## When these rules change

Edit only [`AGENTS.md`](../AGENTS.md) and this file. Keep the directory-level
`AGENTS.md` files in sync with the structure they describe.
