# <@ instance_repo.split('/')[-1] @> — Agent & Contributor Conventions

Canonical instructions for AI agents and contributors working in **this
instance repo** — an LKE Landing Zone deployment scaffolded from
[`<@ upstream_org @>/lke-landing-zone`](https://github.com/<@ upstream_org @>/lke-landing-zone)
and driven by the **`llz`** CLI. Pinned to template release **`<@ llz_version @>`**
([`.template-version`](.template-version)).

> **Canonical agent guidance.** This file is discovered directly by Claude Code,
> Codex CLI, and Gemini CLI. Edit this file only — do not duplicate its content
> into stubs.

> **Nested scope.** [`terraform-iac-bootstrap/AGENTS.md`](terraform-iac-bootstrap/AGENTS.md)
> **overrides** this file inside that directory (it is Terraform-only, with extra
> state-safety guards). Read it before editing under `terraform-iac-bootstrap/`.

## Critical constraints

- **The spec is the source of truth.** You edit `landingzone.yaml` +
  `environments/<env>.yaml`; `llz render` reconciles them into the per-deployment
  `terraform-iac-bootstrap/*.tfvars` and the `apl-values/<env>/` overlay. **Never
  hand-edit the rendered `*.tfvars` or `apl-values/<env>/` files** — a re-render
  overwrites them, and `llz render --check` guards them in CI. Change inputs in
  the spec, then re-render.
- **`.template-manifest` is the ownership map.** `managed` files are template-owned
  and overwritten on `llz upgrade` — do not hand-edit them; fixes belong upstream.
  `owned` files are yours. `merge` files carry fork-local tokens 3-way-merged on
  update. Classify a path with `llz ci template-manifest --classify <path>` if
  unsure.
- **Never commit real secrets, API tokens, or kubeconfig files.** All credentials
  flow via GitHub Actions secrets/variables and `llz tokens` at CI time; the
  `.llz/` credential cache and Terraform state are gitignored — keep them so.
- **NEVER attribute commits to Claude or any AI agent.** Do not add a
  `Co-Authored-By:` trailer (or any model/agent attribution) to commit messages,
  and do not set the git author or committer to an agent identity or PR Bodies.
- **Do NOT make code or config changes without explicit approval.**

## Layout

| Path | What it is |
|---|---|
| [`landingzone.yaml`](landingzone.yaml) | Instance identity + shared `spec.defaults` + shared VPCs |
| [`environments/`](environments/) | One `<env>.yaml` cluster definition per deployment |
| [`terraform-iac-bootstrap/`](terraform-iac-bootstrap/) | Terraform roots; the `*.tfvars` are **rendered** from the spec |
| [`apl-values/`](apl-values/) | The apl-core values overlay, per deployment |
| [`docs/`](docs/) | A copy of the landing-zone docs (canonical copy lives upstream) |
| [`.github/workflows/`](.github/workflows/) | Build / bootstrap / health workflows (thin callers of the reusable `llz-*` workflows) |

## Common flow

```bash
llz env add <env> --region <linode-region> --obj-cluster <obj-cluster>  # author the spec + render
llz doctor --env <env>     # readiness gate — fill anything it flags
llz up <env> --yes         # tokens → doctor → build (stops at the first failure)
llz status <env>           # OpenBao / ArgoCD / ESO convergence
llz upgrade                # pull a newer template release (re-render + re-pin)
```

Run `llz <command> --help` for any command. The full adopter path lives in
[docs/quickstart.md](docs/quickstart.md); non-obvious gotchas are in
[docs/adopter-guide.md](docs/adopter-guide.md).

## Before committing

- Re-render after any spec change (`llz render <env>`) and confirm
  `llz render --check` is clean — CI fails on rendered drift.
- Run the instance lint gate (`llz lint`) — it covers Terraform (`tofu fmt`,
  `tflint`, `checkov`), Kubernetes, and `.github/workflows`. The pre-commit hook
  (`llz hooks`) enforces the secrets guard + lint at commit time.
