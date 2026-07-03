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

## 1. Prerequisites

You must have these before you start — the platform assumes them and does not
provision them:

| Prerequisite | Why | Notes |
|---|---|---|
| **Linode account with LKE-Enterprise** | The cluster, VPC, Object Storage, and Cloud Firewalls are all Linode | LKE-E (`+lke` k8s versions), not standard LKE. Production accounts need an executive sponsor + InfoSec approval — start this first (longest lead time); follow the [Linode account request checklist](infosec/linode-account-request-checklist.md) |
| **Akamai App Platform (apl-core) entitlement** | We build *on* the platform it provides (Istio, Argo CD, cert-manager, Harbor, Keycloak) | Pinned via `apl_chart_version`; verify with `helm search repo apl/apl --versions` |
| **A GitOps repo reachable over HTTPS** | apl-core's values schema requires an HTTPS Git URL that every node can reach | Must be reachable over HTTPS by every node — use github.com, gitlab.com, or an internal HTTPS mirror |
| **A fork of this repo** | The TF-managed bootstrap Argo CD Application tracks *your* first-party repo over SSH | See §5 for the literals to repoint |
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
module `git::` host in the four TF roots.

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
`?ref=`, the reusable-workflow `uses:@`, and `template-ref:` are rendered from the
copier `llz_version` and move in lockstep. You adopt a new umbrella release by
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
apl_chart_version = "6.0.0"
```

Renovate keeps the *published artifacts* current. For the **copied** scaffolding
(workflows, overlays), `llz env add` / `llz upgrade` stamp a committed
`.template-version` recording the template repo/ref/commit you generated from.

`llz upgrade` also applies `.template-removals` after the `copier update` —
`copier` never deletes a file the template dropped between versions, so the
template lists obsolete paths there and the upgrade removes them (`untrack` =
`git rm --cached`, keep on disk, for gitignored regenerated artifacts like the
per-env tfvars; `delete` = `git rm`, for a file the template no longer ships).
Idempotent, so re-running is safe; review + commit the resulting removals.

The
Scheduled Checks workflow's `template-drift` job (monthly) reports how far behind
the template your instance has fallen (run `llz drift` for the same check locally). After you pull upstream template
changes, re-run `llz ci stamp-template-version` and commit the refreshed stamp so
the baseline advances. Point it at the upstream template with an `upstream` git
remote or pass `--repo <owner/repo>`; `git remote add upstream <template-repo-url>`.

## 3. The values contract (what you must set)

> **A `landingzone.yaml` spec is required.** You do not hand-write the per-env
> tfvars or the `apl-values/<env>/values.yaml` — `llz env add` / `llz render`
> generate both from the spec (`environments/<env>.yaml` + instance-wide
> `landingzone.yaml`). The tfvars are gitignored build artifacts; the values.yaml
> overlay is committed with its identity, object-store wiring, and values-repo
> coordinates already resolved from the spec (only the runtime secrets are left as
> `${…}` for the cluster-bootstrap apply to fill). There is no non-spec path — an
> instance that never runs `llz render` would ship a values.yaml with literal
> `${…}` strings. The tables below are the **spec fields** behind each tfvar, for
> reference; you set them in the spec (§4), not by editing tfvars.

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

### `cluster-bootstrap/` — install apl-core + seed GitOps creds

| Variable (spec field) | Class | Notes |
|---|---|---|
| `region`, `apl_values_env` (env name) | MUST-SET | Deployment discriminator; must match the cluster workspace + `apl-values/<env>` dir |
| `cluster.bootstrap.name` | MUST-SET | → apl-core `cluster.name` (Istio hosts, Argo context). Written straight into values.yaml by `llz render` — **no longer a cluster-bootstrap tfvar** |
| `cluster.bootstrap.domainSuffix` | MUST-SET | → apl-core `cluster.domainSuffix`. Written into values.yaml by `llz render`; `llz ci resolve-harbor-url` derives `harbor.<domain>` from the spec directly (no `cluster_domain` tfvar). Per-env prefix so siblings don't collide |
| `cluster.bootstrap.aplValues.repoURL` (`apl_values_repo_url`) | MUST-SET | **HTTPS**, publicly reachable (see §1). `llz render` writes `otomi.git.repoUrl`; the tfvar also feeds the Argo CD values-repo credential Secret |
| `cluster.bootstrap.aplChartVersion` | MUST-SET | Pin deliberately |
| `cluster.bootstrap.aplValues.revision` / `.username`, `appsRepoRevision` | default | `revision`/`username` → `otomi.git.branch`/`username` in values.yaml (`revision` defaults to a per-env **`apl-<env>`** branch that apl-core owns and pushes to — kept off `main`, see [apl-core-values-branch-isolation.md](designs/apl-core-values-branch-isolation.md); `username` defaults to `x-access-token`); the values-repo `revision` is **no longer a tfvar** |
| The Loki/Harbor S3 bucket names + endpoint | derived | `llz render` derives them from the env name + `cluster.objectStorage.cluster` and writes them into values.yaml — **not a cluster-bootstrap tfvar** |
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
copier copy --trust --vcs-ref v0.1.0 -d llz_version=v0.1.0 \
  gh:akamai-consulting/lke-landing-zone my-instance
# Copier asks for:
#   upstream_org   — the org hosting the LLZ template/modules/charts (default
#                    akamai-consulting; set to your fork if you publish your own)
#   instance_repo  — this instance's own <owner>/<name>
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
llz upgrade --ref v0.2.0   # preferred: copier update + re-pin to v0.2.0 in lockstep
# or, raw copier (re-pin the version yourself):
copier update --trust --vcs-ref v0.2.0 -d llz_version=v0.2.0
```

Copier re-renders the old and new template versions and applies only the delta,
so your local edits survive — conflicts appear (as `.rej`/merge markers) **only**
where you changed a line the template also changed. The same `--trust`-gated task
re-runs on update, so `docs/` refreshes to the new template version too. What
gets overwritten vs. merged vs. left alone follows `.template-manifest` (managed /
merge / owned);
`terraform/*/.terraform.lock.hcl` files are seeded
once and never re-touched (`_skip_if_exists` in `copier.yml`). This is the clean
counterpart to the **versioned-artifact** track (Renovate bumps the
independently-versioned OCI charts + external action digests — §2): `llz upgrade`
moves the *scaffold and the first-party LLZ pins* (module `?ref=`, workflow
`uses:@`/`template-ref:`, rendered from `llz_version` in lockstep), while Renovate
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

### Adding a deployment (environment) inside an instance

Use `llz env add` instead of hand-copying overlays. It declares the env in the
LandingZone spec and `llz render`s a thin overlay over the shared apl-values base
(`_shared/` + `components/`) — no per-env clone to keep in sync — swapping its
identity tokens (env name, `cluster.name`, domain suffix, Linode region, OBJ
cluster). The scaffolding is built into the binary, so it works in an
instance with no scripts/ tree:

```bash
# preview first — writes nothing
llz env add <env> --region us-sea --obj-cluster us-sea-1 --dry-run

# then create the scaffold (must-set values can be passed as flags up front)
llz env add <env> --region us-sea --obj-cluster us-sea-1 \
  --k8s-version v1.33.6+lke7 --acl-inventory-repo my-org/ip-inventory
```

It generates `terraform-iac-bootstrap/{cluster,cluster-bootstrap,object-storage}/<env>.tfvars`
(**gitignored** build artifacts — regenerated from the spec on every render and in CI, so you
commit only the spec + overlay) and the `apl-values/<env>/` overlay, then prints the values
you must still fill (region, `k8s_version`, `apl_values_repo_url`, `obj_cluster`)
and scans for leftover template tokens to
review. Validate the overlay renders:

```bash
kubectl kustomize apl-values/<env>/manifest >/dev/null   # must succeed
```

## 5. Org literals to repoint to your fork

**Everything inside `instance-template/` is repointed by Copier — you don't
hand-edit it.** `copier copy`/`copier update` fill the two scaffold-level tokens
for you: `upstream_org` (every `akamai-consulting` in the scaffold — module
`git::` sources, the OCI charts registry at `cluster-bootstrap/main.tf`, every
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
3. **Install apl-core** — `module=cluster-bootstrap`. Helm-installs apl-core and
   applies the `apl-values/<env>/manifest` Argo CD Applications.
4. **Converge** — the workflow polls ``llz ci converge`` (wrapping
   ``llz ci health``) until the cluster meets the convergence contract.
5. **Bootstrap OpenBao** — dispatch `.github/workflows/bootstrap-openbao.yml` for
   the env: seed the static seal key, `bao operator init` (recovery keys; the pods
   auto-unseal from the static seal key), then `llz ci bao-configure` writes the KV
   engine, auth methods, and policies.
6. **DNS** — no dedicated step. The `llz-letsencrypt-*` ClusterIssuers sync
   automatically via Argo CD (they live in the mandatory `_shared/manifest/dns`
   base). DNS-01 challenges are solved by apl-core's `cert-manager-webhook-linode`,
   which holds its own Linode token (`TF_VAR_linode_dns_token` from the
   `LINODE_DNS_TOKEN` secret, applied at the `cluster-bootstrap` TF apply) — no
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
