# <@ instance_repo.split('/')[-1] @>

An **LKE Landing Zone** instance — a converging LKE-Enterprise + Akamai App
Platform (apl-core) cluster, scaffolded from
[`<@ upstream_org @>/lke-landing-zone`](https://github.com/<@ upstream_org @>/lke-landing-zone)
and driven by the **`llz`** CLI. Pinned to template release **`<@ llz_version @>`**
([`.template-version`](.template-version)).

> **The spec is the source of truth.** You edit `landingzone.yaml` +
> `environments/<env>.yaml`; `llz render` reconciles them into the per-deployment
> tfvars and `apl-values/<env>/` overlay. **Never hand-edit the rendered
> `*.tfvars`** — a re-render overwrites them, and `llz render --check` guards them in CI.

## What's in here

| Path | What it is |
|---|---|
| [`landingzone.yaml`](landingzone.yaml) | Instance identity + shared `spec.defaults` + shared VPCs |
| [`environments/`](environments/) | One `<env>.yaml` cluster definition per deployment |
| [`terraform-iac-bootstrap/`](terraform-iac-bootstrap/) | Terraform roots; the `*.tfvars` are **rendered** from the spec |
| [`apl-values/`](apl-values/) | The apl-core values overlay, per deployment |
| [`docs/`](docs/) | A copy of the landing-zone docs (canonical copy lives upstream) |
| [`.github/workflows/`](.github/workflows/) | Build / bootstrap / health workflows (thin callers of the reusable `llz-*` workflows) |

## Quick start

Install the CLI (or `llz self-update` if you already have it), then drive the flow:

```bash
curl -fsSL https://raw.githubusercontent.com/<@ upstream_org @>/lke-landing-zone/main/template-scripts/install-llz.sh | bash

llz env add <env> --region <linode-region> --obj-cluster <obj-cluster>  # author the spec + render
llz doctor --env <env>     # readiness gate — fill anything it flags
llz up <env> --yes         # tokens → doctor → build (stops at the first failure)
llz status <env>           # OpenBao / ArgoCD / ESO convergence
```

Full walkthrough — accounts, credentials, bootstrap order — is in
[docs/quickstart.md](docs/quickstart.md).

## Common commands

| Command | Does |
|---|---|
| `llz env add <env> …` | author a deployment (`environments/<env>.yaml`) + render |
| `llz env set <env> k=v` | change a per-env field + re-render |
| `llz env show <env>` | effective config after `spec.defaults` merge |
| `llz render <env> --diff` | preview the files a render would change |
| `llz doctor --env <env>` | the single "am I ready to build?" gate |
| `llz tokens --env <env> --yes` | provision credentials (state bucket/key, PATs) |
| `llz up <env> --yes` | tokens → doctor → build |
| `llz status <env>` | cluster health |
| `llz upgrade` | pull a newer template release (re-render + re-pin) |

Run `llz <command> --help` for any command.

## Docs

Shipped locally under [`docs/`](docs/):

- [Quick start](docs/quickstart.md) — nothing → converged cluster, the fast path
- [Adopter guide](docs/adopter-guide.md) — the same path with full rationale
- [Landing-zone spec](docs/landing-zone-spec.md) — the `landingzone.yaml` + `environments/<env>.yaml` model
- [Environments & promotion](docs/environments-and-promotion.md) — deployments, HA pairs, dev→staging→prod
- [Secrets](docs/secrets.md) + [OpenBao bootstrap runbook](docs/runbooks/bootstrap-openbao.md)
- [Dev Container](docs/devcontainer.md) — a prebuilt workstation with the whole toolchain

The always-current source of these docs lives upstream:
[`<@ upstream_org @>/lke-landing-zone/docs`](https://github.com/<@ upstream_org @>/lke-landing-zone/tree/main/docs).

## Staying current

This instance pins to template release **`<@ llz_version @>`**. Two independent tracks:

```bash
llz self-update      # get the new CLI first (the version anchor)
llz upgrade          # re-render the scaffold + re-pin the module/workflow refs
llz drift            # how far behind the template head are you?
```

`llz upgrade` moves the scaffold and the first-party LLZ pins in lockstep; **Renovate**
PRs move the independently-versioned OCI charts + external Action digests. See
[docs/quickstart.md §5](docs/quickstart.md).
