# System Team Adopter's Guide

> **Audience:** a sister **system team** on the same stack — Linode LKE-Enterprise +
> Akamai App Platform (apl-core) — that wants to stand up its own self-hosted
> platform from this repo's reusable artifacts. The scope is deliberate: apl-core
> and Linode LKE-Enterprise are hard givens, not abstracted away — only org/cluster
> identity is variabilized.
>
> **This is not a fork-and-pray guide.** The durable units of reuse are *published*
> and independently versioned: Helm charts as OCI artifacts on GHCR, Terraform
> modules as tagged `git::` sources. The monorepo consumes its own published
> artifacts; you do the same, overriding only the org/cluster identity that differs
> between sibling deployments.
>
> **Just want to get going?** [quickstart.md](quickstart.md) drives the whole
> path with the **`llz`** CLI (token wizard + `copier` + `gh`) — accounts →
> tokens → instance → environment → secrets → build → upgrades. This guide is the
> same path with the rationale spelled out; every `llz` command maps to a step
> below.

---

<!-- toc -->
## Contents

- [1. Prerequisites](#1-prerequisites)
- [2. The reusable artifacts](#2-the-reusable-artifacts)
- [3. The values contract (what you must set)](#3-the-values-contract-what-you-must-set)
- [4. Scaffold an instance, and pull template updates — Copier](#4-scaffold-an-instance-and-pull-template-updates-copier)
- [5. Org literals to repoint to your fork](#5-org-literals-to-repoint-to-your-fork)
- [6. Bootstrap order](#6-bootstrap-order)
- [7. Checklist](#7-checklist)

<!-- /toc -->

## 1. Prerequisites

You must have these before you start — the platform assumes them and does not
provision them:

| Prerequisite | Why | Notes |
|---|---|---|
| **Linode account with LKE-Enterprise** | The cluster, VPC, Object Storage, and Cloud Firewalls are all Linode | LKE-E (`+lke` k8s versions), not standard LKE. Production accounts need an executive sponsor + InfoSec approval — start this first (longest lead time); follow the [Linode account request checklist](infosec/linode-account-request-checklist.md) |
| **Akamai App Platform (apl-core) entitlement** | We build *on* the platform it provides (Istio, Argo CD, cert-manager, Harbor, Keycloak) | Pinned via `apl_chart_version`; verify with `helm repo add apl https://linode.github.io/apl-core && helm repo update && helm search repo apl/apl --versions` (the `repo add` is required first — without it `search repo` reports no results rather than an error) |
| **A GitOps repo reachable over HTTPS** | apl-core's values schema requires an HTTPS Git URL that every node can reach | Must be reachable over HTTPS by every node — use github.com, gitlab.com, or an internal HTTPS mirror |
| **Your own instance repo** | The TF-managed bootstrap Argo CD Application tracks *your* repo over SSH; `gh` targets it | Created for you by `llz new --push` (the `instance_repo` copier answer) — but its **owner must already exist**: `llz new` creates the repository, never the GitHub org that holds it, so [create the org](https://github.com/organizations/new) first (or answer `instance_repo` with your own user). **Forking this template is *not* required** — `upstream_org` defaults to `akamai-consulting` and the charts are public on GHCR. Fork only if you want to publish your own artifacts; then see §5 |
| **GHCR pull access** | Argo CD pulls the first-party charts from `ghcr.io/<org>/charts` | The packages are **public** — Argo CD pulls them anonymously, no credential needed. (A private fork can still seed a repo credential from `GHCR_READ_TOKEN` + `GHCR_USERNAME`; the Terraform gate honors it when set.) |
| CLI tooling | `terraform`/`tofu`, `kubectl`, `helm`, `linode-cli`, `gh`, `bao`, `jq` | **`llz doctor` is the authoritative, always-current list** + reports which are installed and whether `gh` is authed. Skip the host installs by working in the [Dev Container](devcontainer.md), which ships them all. |

## 2. The reusable artifacts

You consume two published artifact sets — you do **not** copy their source:

- **Helm charts** → OCI on GHCR. Argo CD Applications reference
  `oci://ghcr.io/<org>/charts/<chart>:X.Y.Z`. Contract + chart list:
  [kubernetes-charts/README.md](../kubernetes-charts/README.md).
- **Terraform modules** → tagged `git::` sources. Roots pin
  `git::ssh://…/<repo>.git//terraform-modules/<name>?ref=vX.Y.Z` (the one umbrella
  release tag). Contract + release process:
  [terraform-modules/RELEASING.md](../terraform-modules/RELEASING.md).

Upstream fixes reach you via version bumps, not manual diffs. To point at your own
fork/registry, override the chart `gitRepoURL`/`chartsRegistry` values (§5) and the
module `git::` host in the generated TF roots.

### Keeping the pins current — Renovate

`instance-template/renovate.json` ships into your instance repo and **automates the
version bumps** so "fixes reach you via version bumps" doesn't mean bumping by hand.
Enable Renovate (the GitHub App or self-hosted) on the instance repo and it opens PRs
for:

- **OCI Helm charts** — the `argocd` manager bumps `targetRevision` on Argo CD
  Applications under `apl-values/<env>/manifest/`; `oci://ghcr.io/<org>/charts/llz-*`
  is registered via `helmv3.registryAliases`. Charts version independently, so
  Renovate owns these bumps.
- **External actions** — third-party `uses:` are pinned to digests
  (`helpers:pinGitHubActionDigests`) and kept current automatically.

The **first-party LLZ pins are NOT Renovate-managed**: the Terraform module
`?ref=` is rendered from the copier `llz_version`, and everything else in CI
reads that same answer at runtime, so they move in lockstep by construction. You adopt a new umbrella release by
`llz self-update` (get the new CLI) then `llz upgrade` (re-renders every first-party
pin to that version) — the CLI is the version anchor. Renovate is deliberately
disabled on these so it never races `llz upgrade` (the `enabled: false` rule in
`renovate.json`).

First-party chart patch bumps automerge; everything else lands as a grouped weekly
PR ("LLZ platform artifacts"). **After forking, repoint** the `packageName` /
`registryAliases` in `renovate.json` from `akamai-consulting` to your fork/registry —
the same repoint you do for the module `git::` host.

For an **upstream chart** whose version lives in tfvars (e.g. `apl_chart_version`),
add a one-line annotation above it so the annotation manager bumps it too:

```hcl
# renovate: datasource=helm depName=apl registryUrl=https://<your-apl-helm-repo>
apl_chart_version = "v6.1.0"
```

Renovate keeps the *published artifacts* current. For the **copied** scaffolding
(workflows, overlays), the template repo/ref you generated from is recorded once,
by copier, in `.copier-answers.yml` — `llz drift` and CI both read it there. (LLZ
used to write a second copy to a committed `.template-version`; `llz upgrade`
deletes that orphan.)

`llz upgrade` also applies `.template-removals` after the `copier update` —
`copier` never deletes a file the template dropped between versions, so the
template lists obsolete paths there and the upgrade removes them (`untrack` =
`git rm --cached`, keep on disk, for gitignored regenerated artifacts like the
per-env tfvars; `delete` = `git rm`, for a file the template no longer ships).
Idempotent, so re-running is safe; review + commit the resulting removals.

The
Scheduled Checks workflow's `template-drift` job (monthly) reports how far behind
the template your instance has fallen (run `llz drift` for the same check locally).
There is no stamp to refresh by hand — `llz upgrade` re-records the pin in
`.copier-answers.yml`, which is the one place both `llz drift` and CI read it from.
Point the check at the upstream template with an `upstream` git remote
(`git remote add upstream <template-repo-url>`) or `llz drift --repo-url <url>`.

## 3. The values contract (what you must set)

> **A `landingzone.yaml` spec is required.** You do not hand-write the per-env
> tfvars or the `apl-values/<env>/` overlay — `llz env add` / `llz render` generate
> both from the spec (`environments/<env>.yaml` + instance-wide `landingzone.yaml`).
> The tfvars are gitignored build artifacts; the overlay (the `manifest/`
> kustomization and the `apl-overlay/` app+obj toggles) is committed. There is no
> non-spec path.
>
> Note that **apl-core's own `apl-values/values.yaml` is NOT rendered** — on the
> managed App Platform Linode owns it (ADR
> [0005](adr/0005-managed-app-platform.md)), and the scaffold CI check fails the
> build if a render ever emits one. The tables below are the **spec fields** behind each tfvar, for reference; you set
> them in the spec (§4), not by editing tfvars.

**SECRET** values still come from `TF_VAR_*` environment variables at apply time and
are never committed. Everything else is a Linode/apl-core default you usually keep.

### `cluster/` — the LKE-E cluster, VPC, node pool, firewall

| Variable | Class | Notes |
|---|---|---|
| `cluster_label`, `region`, `k8s_version` | MUST-SET | Cluster identity + Linode region + an LKE-E version live in your account |
| `github_runner_ipv4_cidrs` / `*_ipv6_cidrs` | optional | Static operator/CI/VPN egress CIDRs that seed the bootstrap control-plane ACL + node firewall. Leave empty for github.com-hosted runners (they open their egress IP at runtime via `llz ci runner-acl open`). **Never `0.0.0.0/0`** |
| `node_type`, `node_count`, `vpc_subnet_cidr`, HA/audit toggles, autoscaler | default | Linode-shaped defaults; keep unless sizing differs |

> There is **no** `control_plane_acl_enabled`/`control_plane_acl_ipv4` variable at
> this root. Terraform seeds the ACL at create from `github_runner_*` CIDRs so the
> bootstrapping runner can reach the API server; after init the in-cluster
> cloud-firewall-controller owns the ACL — it resolves EAA/bastion CIDRs from the
> Linode firewall template via the Linode API and reconciles every cycle.

### apl-core install — `llz ci bootstrap-cluster` (formerly the `cluster-bootstrap/` TF root)

| Variable (spec field) | Class | Notes |
|---|---|---|
| `region`, env name | MUST-SET | Deployment discriminator; must match the cluster deployment + `apl-values/<env>` dir |
| `cluster.bootstrap.name` | MUST-SET | → apl-core `cluster.name` (Istio hosts, Argo context). → apl-core's own values (Linode-owned on managed) — **not a tfvar, and not rendered by `llz render`** |
| `cluster.bootstrap.domainSuffix` | **MUST NOT SET** | Linode owns the `lke<id>.akamai-apl.net` domain and LLZ discovers it in-cluster; the spec validator **rejects** a value outright (a stale one would misroute the Keycloak issuer + Harbor URL). `llz ci resolve-harbor-url` resolves `harbor.<domain>` from the live cluster |
| `cluster.bootstrap.managedAppPlatform` | MUST-SET (`true`) | LLZ never self-installs apl-core; validated on every env. `llz env add` seeds it into `spec.defaults` for you |
| `cluster.bootstrap.aplValues.repoURL` (`apl_values_repo_url`) | MUST-SET | **HTTPS**, publicly reachable (see §1). → apl-core `otomi.git.repoUrl` (Linode-owned on managed); the tfvar also feeds the Argo CD values-repo credential Secret |
| `cluster.bootstrap.aplChartVersion` | optional | **Omit it.** On managed App Platform Linode owns the deployed apl-core version — bootstrap does not consume this field, so a pin deploys nothing. It survives only as the version `llz ci assert-apl-version` and the `validate-apl-values` schema check resolve; omitted, both use the baseline this llz tracks. Set it only to make that check assert a version other than the baseline |
| `cluster.bootstrap.aplValues.revision` / `.username`, `appsRepoRevision` | default | `revision`/`username` → `otomi.git.branch`/`username` in values.yaml (`revision` defaults to a per-env **`apl-<env>`** branch that apl-core owns and pushes to — kept off `main`, see [apl-core-values-branch-isolation.md](designs/apl-core-values-branch-isolation.md); `username` defaults to `x-access-token`); the values-repo `revision` is **no longer a tfvar** — apl-core owns both on managed |
| The Loki/Harbor S3 bucket names + endpoint | derived | `llz render` derives them from the env name + `cluster.objectStorage.cluster` into the apl-overlay (`apl-values/<env>/apl-overlay/obj.yaml`) — **not a cluster-bootstrap tfvar** |
| `tf_state_bucket`, `linode_dns_token`, `apl_values_repo_token`, `linode_token`, `openbao_secrets_write_token` | SECRET | All via `TF_VAR_*` in CI. `apl_values_repo_token` = fine-grained PAT (Contents: write). (apl-core 6.x auto-generates the Loki admin password — no `loki_admin_password` input.) |

### `object-storage/` — registry + logs OBJ buckets

| Variable | Class | Notes |
|---|---|---|
| `region_suffix` | MUST-SET | Must match the cluster workspace deployment |
| `obj_cluster` | MUST-SET | `linode-cli object-storage clusters-list` |
| `keyRotationDays` | deprecated/ignored | Key rotation is owned by the in-cluster `linodeCredRotator` CronJob (first keys minted at bootstrap by `llz ci mint-bootstrap-objkeys`); the `obj_key_rotation_days` TF variable was removed |
| `linode_token` | SECRET | `TF_VAR_linode_token` |

OpenBao auth/policy/KV configuration is **not** a Terraform root — `llz ci
bao-configure` (run from `bootstrap-openbao.yml` after the cluster is up) is the
sole owner. There are no `openbao_*` tfvars to set.

## 4. Scaffold an instance, and pull template updates — Copier

This template is a [Copier](https://copier.readthedocs.io) template. There are two
layers, and Copier owns the outer one:

- **Instance** (this whole repo): scaffold it once with `copier copy`, and pull
  later template releases with `copier update`.
- **Environment** (a region/deployment *inside* an instance): added with
  `llz env add` — see the subsection below.

```bash
# scaffold a new instance from a template release tag.
# --trust is REQUIRED: Copier tasks (1) copy the operator docs/ into the instance
# (it lives outside the scaffold) and (2) arm the pre-commit hook via `llz hooks`.
# Without --trust, Copier skips both — no docs/, and you arm the hook yourself with
# `llz hooks`. (The bootstrap/operations scripts are NOT copied in — the reusable
# llz-* workflows run them from a template checkout.)
copier copy --trust --vcs-ref v0.0.38 -d llz_version=v0.0.38 \
  gh:akamai-consulting/lke-landing-zone my-instance
# Copier asks for:
#   upstream_org   — the org hosting the LLZ template/modules/charts (default
#                    akamai-consulting; set to your fork if you publish your own)
#   instance_repo  — this instance's own <owner>/<name>
#   openbao_team   — the default team name for scoped, non-root OpenBao writes
#                    (default `platform` → secret/platform; lowercase kebab).
#                    Becomes an apl-core team + a <name>-writer policy; see
#                    landing-zone-spec.md's spec.teams.
#   llz_version    — the release to pin module/workflow refs to. PASS IT EXPLICITLY
#                    (`-d llz_version=<vcs-ref>`) so the pins match the version you
#                    scaffold from; `llz new` sets it automatically. The `main`
#                    default tracks the tip unpinned.
```

> Prefer `llz new` — it sets `llz_version` to its own binary version for you, so
> the scaffold pins to exactly the release the CLI came from.

`copier copy` renders `instance-template/` into the new repo with those tokens
filled in, and writes `.copier-answers.yml` recording the answers + the template
commit. Later, inside the instance:

```bash
llz upgrade --ref v0.0.39   # preferred: copier update + re-pin to v0.0.39 in lockstep
# or, raw copier (re-pin the version yourself — then `llz render`, see below):
copier update --trust --vcs-ref v0.0.39 -d llz_version=v0.0.39
```

> **Prefer `llz upgrade` over raw `copier update`** for more than the re-pin. The
> committed `apl-values/<env>/` kustomizations resolve `?ref=` from the pin, so
> rewriting it invalidates them every time; `llz upgrade` re-runs `llz render`
> after the update, and raw copier does not. Skip that render and ArgoCD keeps
> syncing the previous release's shared manifests — a difference in what is
> *deployed*, not just what is checked in.

Copier re-renders the old and new template versions and applies only the delta,
so your local edits survive — conflicts appear (as `.rej`/merge markers) **only**
where you changed a line the template also changed. The same `--trust`-gated task
re-runs on update, so `docs/` refreshes to the new template version too. What
gets overwritten vs. merged vs. left alone follows `.template-manifest` (managed /
merge / owned);
`terraform-iac-bootstrap/*/.terraform.lock.hcl` files are seeded
once and never re-touched (`_skip_if_exists` in `copier.yml`). This is the clean
counterpart to the **versioned-artifact** track (Renovate bumps the
independently-versioned OCI charts + external action digests — §2): `llz upgrade`
moves the *scaffold and the first-party LLZ pins* (module `?ref=`, rendered from
`llz_version`, which is also the pin CI reads), while Renovate
moves the *independently-versioned charts + actions*.

### Local checks (`llz` + git hooks)

`llz` carries the fast, offline checks of your own content — no template checkout
needed:

```bash
llz lint      # fast gate: tofu fmt-check + tflint + actionlint + gitleaks
llz fmt       # auto-fix: tofu fmt
llz validate  # heavier, on-demand: terraform validate + checkov
llz hooks     # (re-)install the pre-commit hook in this clone
# advanced/debug escape hatch (hidden from top-level help): run one step alone —
#   llz check tf-lint   # see `llz check --help` for the full step list
```

`copier copy --trust` runs `llz hooks` for you, installing a pre-commit hook
(secret-file guard + `llz lint`). The hook is per-clone (not committed), so re-run
`llz hooks` after a fresh `git clone`. Missing linters skip with a warning rather
than blocking a commit, so install what you want for full coverage: `tofu`/
`opentofu`, `tflint`, `actionlint`, `gitleaks` (+ `terraform`, `checkov` for `llz
validate`). The deeper chart/manifest validators (kube-linter, kubeconform, ArgoCD
render checks, ExternalSecret-path validation) need the template's charts and run
in CI via the reusable `llz-*` workflows, not locally.

**Getting updates.** The checks live in the `llz` binary, so they move when you
upgrade `llz` — independent of `copier update`. Only the lint *configs*
(`.tflintrc.hcl`, `.checkov.yaml`, `.gitleaks.toml`) are `managed` template files
that `copier update --trust` re-renders. To extend without fighting updates, use
the `owned` (never-touched) escape hatches:

- `.llz/commands.yaml` — your own `llz` subcommands. See **[Extending llz](extending-llz.md)**.
- `.githooks/pre-commit.local` — extra pre-commit checks (an executable script,
  run by `llz precommit` after the built-in `llz lint`).

### What `llz` checks before it lets you spend money

Each command validates what it can before doing anything expensive, and the
refusal carries the fix. The full set, so you know what is and is not covered:

| Command | Checks | Blocks? |
|---|---|---|
| `llz new <dir>` | the directory is empty (copier renders *on top* of a populated one) | yes |
| `llz env add` | the CWD is an instance root; `--region` and `--obj-cluster` exist in your Linode account and belong together; runner CIDRs parse, match their flag's address family, and are not `0.0.0.0/0` | yes |
| `llz env add` | lists the overlay placeholders left to fill, and names the one command that fixes the rendered ones (`llz spec set instance.repo=…`) | no |
| `llz doctor --env` | spec valid, committed `apl-values` in sync with it, no unfilled placeholders, every required repo secret/variable set; reports an open-world control-plane ACL wherever it came from | yes (advisory for the ACL) |
| `llz build` / `llz up` | `landingzone.yaml` and `environments/<env>.yaml` are on the branch the workflow checks out, and the deployment exists | yes (`--skip-preflight` to override) |
| `llz build` / `llz up` | your working copy differs from the pushed spec, or the deployment is only on the remote | no — the build uses the pushed tree either way |
| `llz status` | a kubeconfig exists and the cluster answers, before running any check against it | yes |

Two properties are worth relying on:

- **A check that cannot get an answer does not block you.** Without `gh`, a
  Linode token, a reachable API or a spec, each check says it is skipping and the
  command proceeds.
- **`workflow_dispatch` runs from the repo's default branch.** Pushing a feature
  branch does not put a deployment where the build reads it; the refusal says so
  and names your branch.

### Adding a deployment (environment) inside an instance

Use `llz env add` instead of hand-copying overlays. It declares the env in the
LandingZone spec and `llz render`s a thin overlay over the shared apl-values base
(`platform-apl/manifest` + `components/`) — no per-env clone to keep in sync — swapping its
identity tokens (env name, `cluster.name`, domain suffix, `REGION_SHORT`, Linode
region, OBJ cluster). The scaffolding is built into the binary, so it works in an
instance with no scripts/ tree:

```bash
# preview first — writes nothing
llz env add <env> --region us-sea --obj-cluster us-sea-1 --dry-run

# then create the scaffold (must-set values can be passed as flags up front)
llz env add <env> --region us-sea --obj-cluster us-sea-1 \
  --k8s-version v1.33.6+lke7 --promotion-rank 1
```

It generates `terraform-iac-bootstrap/{cluster,object-storage,databases}/<env>.tfvars`
(**gitignored** build artifacts — regenerated from the spec on every render and in CI, so you
commit only the spec + overlay) and the `apl-values/<env>/` overlay. `--region` and
`--obj-cluster` are **required** and the rest fall back to `spec.defaults`, so
nothing is left half-filled; it then lists any residual `apl-values` placeholders
for you to fill. Validate the overlay renders:

```bash
kubectl kustomize apl-values/<env>/manifest >/dev/null   # must succeed
```

## 5. Org literals to repoint to your fork

**Everything inside `instance-template/` is repointed by Copier — you don't
hand-edit it.** `copier copy`/`copier update` fill the two scaffold-level tokens
for you: `upstream_org` (every `akamai-consulting` in the scaffold — module
`git::` sources, the OCI charts registry pin in the spec, every
Argo CD Application's `repoURL: ghcr.io/<org>/charts`, CI images) and
`instance_repo` (the bootstrap Application repo URL + `gh` targeting). The
workflows need no repointing at all: the reusable bodies and composite actions
are vendored into the instance and referenced with repo-local `./` paths
(ADR 0003), so they carry no org.
Copier renders every file in-place, so those resolve to your fork on render.

The only first-party references you repoint by hand live **OUTSIDE** the scaffold,
in the published `kubernetes-charts/` chart values (which Copier doesn't template):

| Where | What | Change to |
|---|---|---|
| `kubernetes-charts/llz-argo-bootstrap-apps/values.yaml` | `gitRepoURL: "REPLACE_ME-git-repo-url"` | Your GitOps repo URL (intentional placeholder) |
| `kubernetes-charts/llz-cert-automation/values.yaml` + its Application overlay | `githubDeploy.repo`, `harborUrl` | Your repo / Harbor host |

These are overridable values/literals, not abstraction seams — the platform stays
Linode + apl-core shaped by design.

## 6. Bootstrap order

The bootstrap is GitHub-Actions-driven (there is no single `bootstrap.sh`). For a
new env, in order:

1. **Provision the cluster** — dispatch the Terraform workflow
   (`.github/workflows/terraform.yml`) with `action=apply`, `module=cluster`,
   `region=<env>`. Creates the LKE-E cluster, VPC, firewall, node pool.
2. **Object storage** — `module=object-storage` for the registry/log buckets.
3. **Install apl-core** — folded into `module=all`, which runs `llz ci bootstrap-cluster` after the cluster apply. Helm-installs apl-core and
   applies the `apl-values/<env>/manifest` Argo CD Applications.
4. **Converge** — the workflow polls ``llz ci converge`` (wrapping
   ``llz ci health``) until the cluster meets the convergence contract.
5. **Bootstrap OpenBao** — dispatch `.github/workflows/bootstrap-openbao.yml` for
   the env: seed the static seal key, `bao operator init` (recovery keys; the pods
   auto-unseal from the static seal key), then `llz ci bao-configure` writes the KV
   engine, auth methods, and policies.
6. **DNS** — no dedicated step. The `llz-letsencrypt-*` ClusterIssuers come from
   the managed App Platform and sync automatically via Argo CD.
   DNS-01 challenges are solved by apl-core's `cert-manager-webhook-linode`,
   which holds its own Linode token (`TF_VAR_linode_dns_token` from the
   `LINODE_DNS_TOKEN` secret, rendered into apl-core's values by `llz ci bootstrap-cluster`) — no
   OpenBao seed or ExternalSecret is involved. (The Argo CD / apl-core values-repo
   credential is the `APL_VALUES_REPO_TOKEN` PAT, provisioned by `llz tokens`.)

See [docs/runbooks/](runbooks/) for per-step detail (`bootstrap-openbao.md`,
`apl-values-propagation.md`) and [docs/playbooks/operator-onboarding.md](playbooks/operator-onboarding.md)
for day-2 operations.

## 7. Checklist

- [ ] Prerequisites in §1 satisfied (LKE-E, apl-core, HTTPS GitOps repo, fork, inventory)
- [ ] `llz env add <env>` run; the three tfvars + overlay generated; `llz doctor --env <env>` green
- [ ] All ADOPTER-MUST-SET values filled (§3); secrets wired as `TF_VAR_*` in CI
- [ ] Org literals repointed to your fork/registry (§5)
- [ ] `kubectl kustomize apl-values/<env>/manifest` succeeds
- [ ] Bootstrap workflows run in order (§6); cluster converges; OpenBao bootstrapped
