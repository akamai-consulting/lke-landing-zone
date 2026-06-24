# Quick Start

> **Goal:** go from nothing to a converging LKE-Enterprise + apl-core cluster
> built from this template, driving the whole flow with the **`llz`** CLI.
>
> **Audience:** a system team standing up its own instance. You're expected to
> already be on the stack (Linode LKE-E + Akamai App Platform). For the full
> rationale behind each step, read the [adopter guide](adopter-guide.md); this
> page is the fast path.

## The whole path — copy/paste, top to bottom

Once the [accounts](#1-accounts-you-need) exist, this is the **entire flow in
order**. Run it line by line, swapping `my-instance`, `lab`, the region, and the
OBJ cluster for your own. No clone of this repo required (the installer is a
one-liner; `llz new` creates your own repo). Each step links to the section that
explains it.

```bash
# 0. Authenticate gh FIRST — the installer and every GitHub call below use it (§2)
gh auth login

# 1. Install the llz CLI (§2)
curl -fsSL https://raw.githubusercontent.com/akamai-consulting/lke-landing-zone/main/template-scripts/install-llz.sh | bash

# 2. Scaffold your instance repo + create/push it on GitHub (§3)
llz new my-instance --push --yes
cd my-instance

# 3. Add a deployment — authors the spec, renders the tfvars + apl-values overlay (§3)
llz env add lab --region us-sea --obj-cluster us-sea-1

# 4. Confirm it's ready to build — fill anything doctor flags, then re-run until green (§4)
llz doctor --env lab

# 5. Provision credentials → readiness gate → build, in ONE command (§4)
llz up lab --yes

# 6. AFTER the build, do the two manual steps the bootstrap can't (§4):
#    • copy unseal keys 4 & 5 + the root token (shown once) to offline storage
#    • delete the OPENBAO_ROOT_TOKEN secret from infra-lab if you seeded one
#      (`llz status` flags it every run until you do)

# 7. Finish DNS-01 issuance, then verify convergence (§4)
llz bootstrap dns lab --yes
llz status lab
```

That is the whole thing, start to converged cluster. Step 5's `llz up` chains the
three gates — `tokens → doctor → build` — and **stops at the first failure**, so a
missing token or unfilled placeholder is caught before the expensive apply; you can
run those three individually to inspect each gate (§4). `llz` itself is a thin
[cobra](https://github.com/spf13/cobra) front-end over the tools this flow already
uses (`copier`, `gh`, `kubectl`, the Linode API) — it sequences them and adds the
`llz tokens` provisioning wizard (state bucket + scoped key, ArgoCD deploy key,
GitHub PATs behind pre-filled links), pushing everything to your repo.

Run `llz <command> --help` for any command; the persistent flags `--dry-run`
(print, change nothing), `--open` (open links), and `--yes` (execute
cloud-mutating commands) work anywhere on the line. Stuck on a step? `llz doctor`
(§4) is the always-current readiness check.

---

## 1. Accounts you need

`llz` can't create these — get them first. The full table (the *why* + where to
get each) is canonical in the [adopter guide §1](adopter-guide.md#1-prerequisites);
the short version:

- **Linode account with LKE-Enterprise** — `+lke` versions, not standard LKE
- **Akamai App Platform (apl-core) entitlement**
- **A GitHub org + an instance repo** — a fork of the template org, or your own

> **Start the Linode account first — it has the longest lead time.**

Run **`llz doctor`** any time to check your CLI tooling + `gh` auth — it is the
authoritative, always-current list of what the flow needs. With a repo/env it
also reports deployment + e2e readiness (see §4).

---

## 2. Install `llz`

**Authenticate `gh` first.** The install script and every `llz` command that
touches GitHub (`llz new`, `llz tokens`, `llz doctor`, `llz self-update`) drive
the `gh` CLI, so it must be logged in *before* you run any of them:

```bash
gh auth login        # one-time; `gh auth status` confirms you're logged in
```

> **Already use `gh` for another host?** The installer and `llz doctor` scope
> their auth check to **`github.com`** (override with `GH_HOST`), so a second
> account in a broken state — e.g. an expired GHE token — won't block you. You
> only need `gh auth status --hostname github.com` to pass; a global
> `gh auth status` failing on an *unrelated* host is fine. If that host is the
> one you want, fix it with `gh auth login --hostname <host>` (or forget it:
> `gh auth logout --hostname <host>`).

> **`gh auth` ≠ your cloud/PAT credentials.** Logging in to `gh` covers GitHub
> repo, release, and API calls only. `llz tokens` (§4) still prompts you for a
> **Linode PAT** and a couple of **GitHub PATs** — that's by design, not a
> re-auth loop; those are the secrets it pushes into your repo so the build can
> run. See §4 for the full list.

**Install it — no clone required.** `llz` ships as a release binary of the public
template repo, [`github.com/akamai-consulting/lke-landing-zone`](https://github.com/akamai-consulting/lke-landing-zone/releases/latest).
Pipe the installer straight from `main`; it picks your platform, resolves the
latest full release, verifies the checksum, and installs to **`~/.local/bin`** —
a per-user dir that needs no `sudo` and works on locked-down corporate machines
that deny writes to `/usr/local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/akamai-consulting/lke-landing-zone/main/template-scripts/install-llz.sh | bash
llz version
# wget:  wget -qO- https://raw.githubusercontent.com/akamai-consulting/lke-landing-zone/main/template-scripts/install-llz.sh | bash
```

The script still uses `gh` to fetch the release asset, so keep `gh` authenticated
(above) — only the script itself is downloaded anonymously.

> **Already have a template or instance checkout?** Skip the `curl` and run the
> same script from there: `./template-scripts/install-llz.sh` (append `v0.2.0` to
> pin a tag, or prefix `ORG=<fork>`).

> **Put `~/.local/bin` on your `PATH`.** If `llz version` prints "command not
> found", the dir isn't on your `PATH` yet — add it (then restart the shell):
>
> ```bash
> echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc   # or ~/.bashrc
> ```

<details>
<summary><strong>Install by hand</strong> — no checkout, or you prefer the raw commands</summary>

Download the asset for your platform with `gh` and put it on your `PATH`. The
release tag is the bare `<VER>`; the snippet resolves the latest with
`gh release list`:

```bash
# macOS arm64 shown; swap the suffix for your platform:
#   llz-darwin-arm64  llz-darwin-amd64  llz-linux-amd64  llz-linux-arm64
ORG=akamai-consulting            # or your fork's org
VER=$(gh release list --repo "${ORG}/lke-landing-zone" --exclude-pre-releases --limit 1 --json tagName --jq '.[0].tagName')
ASSET=llz-darwin-arm64
BINDIR="$HOME/.local/bin"
mkdir -p "$BINDIR"               # create it FIRST (see the PATH note above)
gh release download "${VER}" --repo "${ORG}/lke-landing-zone" \
  --pattern "${ASSET}" --pattern SHA256SUMS --clobber
grep " ${ASSET}\$" SHA256SUMS | shasum -a 256 -c -   # verify; Linux: sha256sum -c -
install -m 0755 "${ASSET}" "$BINDIR/llz" && rm -f "${ASSET}" SHA256SUMS
llz version
```

**Prefer `curl`?** The repo is public, so the browser download URL works
anonymously — no token, no API asset endpoint:

```bash
ORG=akamai-consulting; ASSET=llz-darwin-arm64
VER=$(gh release list --repo "${ORG}/lke-landing-zone" --exclude-pre-releases --limit 1 --json tagName --jq '.[0].tagName')
BINDIR="$HOME/.local/bin"; mkdir -p "$BINDIR"
curl -fsSL \
  "https://github.com/${ORG}/lke-landing-zone/releases/download/${VER}/${ASSET}" \
  -o "$BINDIR/llz"
chmod +x "$BINDIR/llz" && llz version
```

**`curl: (56) Failure writing output to destination`?** The `-o` directory
doesn't exist. curl opens the output file only once bytes arrive, so a missing dir
surfaces mid-download instead of as a clean "can't create file" — that's why the
`mkdir -p "$BINDIR"` above is mandatory.

</details>

Enable shell completion (cobra-generated):

```bash
llz completion zsh  > "${fpath[1]}/_llz"     # zsh  (then restart the shell)
source <(llz completion bash)                 # bash (add to ~/.bashrc)
```

Once installed, keep the binary current without re-running the download — `llz
self-update` pulls the latest **full** release for your platform (pre-release
candidates are skipped; via `gh`, checksum-verified) and replaces itself in place;
`--ref v0.2.0` targets a specific version, `--dry-run` just reports what it would
install.

> Building from source instead? From a template checkout, `make llz` produces
> `bin/llz`.

> **Don't want to install the toolchain on your laptop at all?** Your instance
> ships a [Dev Container](devcontainer.md): "Reopen in Container" gives you a
> prebuilt, multi-arch image with `llz` itself plus everything `llz doctor`
> checks (`terraform`, `kubectl`, `helm`, `bao`, `copier`, `gh`, `linode-cli`, …)
> already on `PATH` — skip straight to §3.

---

## 3. Scaffold your instance — `llz new` + `llz env add`

Two commands: scaffold the instance repo, then add a deployment to it.

```bash
llz new my-instance --push --yes
cd my-instance
llz env add lab --region us-sea --obj-cluster us-sea-1
```

### Scaffold the instance repo — `llz new`

**Most users don't pass `--org`.** It names the **template to scaffold *from***
and defaults to the public upstream `akamai-consulting/lke-landing-zone` — exactly
what you want unless you maintain your *own fork* of the template, in which case
pass `--org <your-fork-org>`. It is **not** where your instance lands; that's the
`instance_repo` copier answer, created by `--push`. (Pointing `--org` at an org
with no template fork makes copier's HTTPS clone 404, which git surfaces as a
confusing `Username for 'https://github.com':` prompt — `llz new` now preflights
this and tells you to fix `--org` or fork first.)

`llz new` runs `copier copy` to render the instance into `my-instance/` (asks
`upstream_org` and `instance_repo`, writes `.copier-answers.yml`). With
`--push --yes` it also runs `gh repo create <instance_repo> --private --source .
--push`, so the remote repo exists and `llz tokens`/`doctor` work against it
immediately. It does **not** ask for credentials — that's `llz tokens` (§4).

> **The instance pins to the `llz` version you installed.** `llz new` renders the
> scaffold's Terraform-module `?ref=` and reusable-workflow `uses:@`/`template-ref:`
> pins to this CLI's own version — no version to hardcode; pass `--ref vX.Y.Z` only
> to pin to a different release. Everything inside the scaffold is repointed to your
> fork by Copier — the only by-hand repoints are the published `kubernetes-charts/`
> values that live outside the scaffold ([adopter-guide §5](adopter-guide.md#5-org-literals-to-repoint-to-your-fork)).

### Add a deployment — `llz env add` writes the spec

`llz env add` is **spec-first**: it authors the declarative LandingZone spec and
then renders it. The first `env add` creates `landingzone.yaml` (your instance
identity + shared `spec.defaults`, seeded from `.copier-answers.yml`); every
`env add` writes one `environments/<env>.yaml` (a `ClusterDefinition` from your
flags) and runs `llz render` to reconcile the spec into the
`terraform-iac-bootstrap/*/<env>.tfvars` + `apl-values/<env>/` overlay. It then
**prints a checklist of the overlay placeholders** the spec doesn't carry. So you
edit **one file per deployment** — `environments/<env>.yaml` — not three tfvars
roots:

```bash
llz env add lab --region us-sea --obj-cluster us-sea-1 \
  --k8s-version v1.33.6+lke7 --node-type g8-dedicated-8-4 --node-count 5 \
  --runner-ipv4-cidrs 203.0.113.0/24
```

`--region` and `--obj-cluster` are **required** (the spec validates them); the
rest of the must-sets come from flags or are inherited from `spec.defaults`. The
**ADOPTER-MUST-SET** values (full table in
[adopter-guide §3](adopter-guide.md#3-the-values-contract-what-you-must-set)):

- `region` (**required**), `k8sVersion` (an LKE-E `+lke` version) + node sizing (`--node-type`/`--node-count` — default to the seeded `spec.defaults`)
- `--runner-ipv4-cidrs` / `--runner-ipv6-cidrs` → `cluster.apiServerAllowCIDRs` — static operator/CI egress CIDRs that seed the bootstrap control-plane ACL (**never `0.0.0.0/0`**; leave empty for github.com-hosted runners, which open their egress IP at runtime via `llz ci runner-acl open`)
- `cluster.domainSuffix` (`--cluster-domain`, default `<env>.internal`), `--apl-values-repo-url` (**HTTPS**, defaults from `instance_repo`), `--apl-chart-version`. `clusterLabel`/`cluster.bootstrap.name` are derived from your instance name — edit `environments/<env>.yaml` to change them.
- `--obj-cluster` (**required**) — your region's Linode OBJ cluster id (e.g. `us-ord-1`, or a newer-generation `us-ord-10`). List them with `linode-cli object-storage clusters-list`; `env add` validates the shape up front.

### Change, inspect & preview a deployment

To change a deployment, use the spec **write** commands — they edit the YAML in
place (comments preserved) and re-render for you, so the edit→render loop can't be
forgotten:

```bash
llz env set lab cluster.nodePool.count=8                # per-env fields (cluster.*/components.*) + re-render
llz env set lab components.harbor.enabled=false components.observability.retention=30d
llz spec set dns.acmeEmail=ops@example.com              # instance-wide fields (landingzone.yaml) + re-render
llz env edit lab                                        # open $EDITOR, re-render on exit
llz network add prod-ord --region us-ord               # declare a shared VPC; attach with
                                                        #   llz env set <env> cluster.network.vpc=prod-ord
```

A bad path is **rejected and the file left untouched** (no corruption), and `env
set` / `spec set` point you at each other for a mis-targeted field.

Inspect and preview before you commit:

```bash
llz components             # what's toggleable: default state, backends, sizing knobs
llz env show lab           # lab's effective config after spec.defaults + component set
llz render lab --diff      # preview exactly which files a render would create/change
```

For an HA pair, `env add` the active first (it defers the render until both peers
exist), then the standby with a **distinct** `--subnet-cidr`; completing the pair
renders both.

### Confirm readiness — `llz doctor --env`

Then fill any overlay placeholders `env add` listed and confirm readiness:

```bash
llz doctor --env lab   # validates the spec + drift, then scans the overlay for placeholders
```

`llz doctor --env` is the single readiness gate (full breakdown in §4): when a
spec is present it **validates it and confirms the committed `apl-values` are in
sync with it** — so a spec edit you forgot to `llz render` is caught here, not at
build. (`llz validate` runs the same spec check alongside the TF code gate.) Run
it now for the local file checks — the repo-config part fills in once `llz tokens`
has pushed. Or, from a template checkout, run `make instance-test` for a fast,
no-cloud smoke test of the whole instantiation path before paying for a real build.

> **The spec is the source of truth.** `landingzone.yaml` (instance identity +
> shared `spec.defaults` + shared VPCs) plus one `environments/<env>.yaml` per
> deployment (cluster definition + `components` toggles + per-component sizing) are
> what you edit; `llz render` reconciles them into the tfvars + `apl-values/<env>/`
> overlay, and `llz render --check` drift-guards the committed result in CI. See
> [landing-zone-spec.md](landing-zone-spec.md) and the fully-commented
> `landingzone.yaml.example` + `environments/prod-web-ord.yaml.example`.

<details>
<summary><strong>What "environment" means here</strong> — three distinct things</summary>

| Term | What it is | Examples |
|---|---|---|
| **Deployment** (the `<env>` you pass to `llz`) | One cluster's identity: its own Terraform state key (`cluster/<deployment>/…`), tfvars, and `apl-values/<deployment>/` overlay. | `primary`, `secondary`, `staging`, `lab`, `e2e` |
| **`infra-<deployment>` GitHub Environment** | One GitHub Actions *Environment* per deployment, holding that cluster's **infrastructure** secrets (Linode token, TF-state keys, OpenBao unseal keys). Locked to `main`. | `infra-primary`, `infra-staging` |
| **Deploy GitHub Environment** | Actions Environments holding **application** secrets your deploy workflow reads at deploy time. Independent of the regional OpenBao clusters. | `lab`, `staging`, `production` |

A production-grade setup is typically **two deployments in two Linode regions**
(`primary` + `secondary`) for HA — OpenBao runs as two independent clusters with
operator-side dual-write, not cross-region replication ([secrets.md](secrets.md)).
Start with **one** deployment (e.g. `lab`), get it converging, then add the
second. When you run more than one, **always bootstrap the first fully before the
next** — additional clusters read Harbor robot credentials the first cluster's
bootstrap writes ([bootstrap-openbao.md](runbooks/bootstrap-openbao.md#additional-cluster-ordering-constraint)).

Want a `dev → staging → prod` flow? Model each stage as a deployment and rank
them with `promotion_rank` — see
[environments-and-promotion.md](environments-and-promotion.md).

</details>

<details>
<summary><strong>Listing deployments + scaffolding an HA pair</strong></summary>

List the deployments you have scaffolded at any time:

```bash
llz env list          # one deployment name per line
llz env list --json   # ["lab","primary",...] — the same source of truth the CI
                      # matrices use (a `discover` job feeds it into every
                      # per-deployment workflow matrix), so a deployment is
                      # covered by rotation + the scheduled health checks the
                      # moment it's in the spec (or its cluster/<name>.tfvars exists).
llz env list --ha     # only deployments in an OpenBao HA pair (ha_role != standalone)
llz env role lab      # active | standby | standalone (from the spec, else cluster/lab.tfvars)
llz env peer lab      # the deployment paired with lab (errors if standalone)
```

Most deployments are **standalone** (a single self-contained OpenBao — the
`llz env add` default). For a two-cluster HA pair, scaffold both with a shared
`--ha-group`, opposite roles, and **distinct** `--subnet-cidr`s (cross-region
peers can't share a CIDR). `env add` defers the render of the first peer until the
second completes the pair, then renders both:

```bash
llz env add east --region us-sea --obj-cluster us-sea-1 --ha-role active  --ha-group prod --subnet-cidr 10.0.0.0/14
llz env add west --region us-ord --obj-cluster us-ord-1 --ha-role standby --ha-group prod --subnet-cidr 10.4.0.0/14
```

The bootstrap, rotation, and Harbor workflows resolve `ha_role`/peer from the spec
(the committed tfvars are rendered from it) instead of hardcoding which cluster is
which.

</details>

---

## 4. Build it — `llz up`

One command runs the rest of the flow — provision credentials, confirm
readiness, dispatch the apply — and finishes by printing the manual actions only
you can do:

```bash
llz up lab --yes        # tokens → doctor → build   (--dry-run previews the whole chain)
```

It stops at the first failure, so a missing token or unfilled placeholder is
caught before the expensive apply. (Run the three commands individually whenever
you want to inspect each gate — see the collapsible below.)

> **`llz up` is interactive — run it at a terminal, not in CI.** `--yes` authorizes
> the *cloud-mutating* steps; it does **not** make the run unattended. The first
> stage (`llz tokens`) still opens pre-filled browser links and prompts you to
> paste a Linode PAT + GitHub PATs and pick an OBJ cluster. Pass `--skip-tokens`
> once those are already provisioned to get a non-interactive `doctor → build`.

> ⚠️ **After the run, do the two manual steps the bootstrap can't:** copy
> **unseal keys 4 & 5 and the root token** from the job summary to secure offline
> storage (shown once), and delete `OPENBAO_ROOT_TOKEN` from `infra-lab` if you
> set it (`llz status` flags it on every run until you do). See the
> [bootstrap runbook](runbooks/bootstrap-openbao.md#after-first-time-bootstrap--required-operator-actions).

Then finish the deferred DNS bit once its token exists (the ArgoCD deploy key was
already provisioned by `llz tokens`), and verify convergence:

```bash
llz bootstrap dns lab --yes    # cert-manager DNS-01 (needs LINODE_DNS_TOKEN)
llz status lab                 # openbao pods / argocd apps / ESO ClusterSecretStore
```

To add the HA second region, repeat §3–4 with `secondary` (or `staging`),
**after** `lab`/`primary` has fully bootstrapped.

<details>
<summary><strong>Run the gates individually</strong> — what <code>llz up</code> does, step by step</summary>

### Provision the credentials — `llz tokens`

```bash
llz tokens --env lab            # prints the readiness plan + the push plan; changes nothing
llz tokens --env lab --yes      # actually creates/gathers/pushes
```

It is **idempotent** — it reads what's already configured (your repo + local
`.llz/*.env`), prints the readiness plan, and **skips anything already set**.
For what's missing it:

| Step | What it does |
|---|---|
| **Linode token** | reads your Linode PAT (full Read/Write) → `LINODE_API_TOKEN`, and uses it for the next two steps |
| **State bucket** | lists your Linode OBJ clusters, you pick one, then **creates** the state bucket → `TF_STATE_BUCKET`, `TF_STATE_ENDPOINT` |
| **State key** | **creates** a bucket-scoped `read_write` OBJ key → `TF_STATE_ACCESS_KEY`, `TF_STATE_SECRET_KEY` |
| **GitHub PATs** | opens pre-filled links and reads: `OPENBAO_SECRETS_WRITE_TOKEN` (classic PAT, **`repo` + `workflow`** scopes — the build writes the remaining infra secrets with it), `APL_VALUES_REPO_TOKEN` (fine-grained PAT, **Contents: write** on your instance repo — apl-core's external values store; the in-cluster Gitea is obsoleted) |
| **Image vars** | computes `TF_IMAGE` / `KUBE_IMAGE` (`ghcr.io/<org>/ci-{terraform,kubernetes}:<tag>`) |
| **Optional** | offers `LINODE_DNS_TOKEN`, `LOKI_ADMIN_PASSWORD`, `CLOUD_FIREWALL_TOKEN` (Enter to skip — the cluster still bootstraps) |

It writes everything to `my-instance/.llz/` (mode `0600`, **gitignored**), then
pushes: secrets into the `infra-lab` GitHub Environment, variables at repo level.

The remaining infra secrets — `OPENBAO_UNSEAL_KEY_*`, the OpenBao root token,
Loki/Harbor OBJ keys, Harbor robots, AppRole IDs — are written **by the build**
(that's exactly what `OPENBAO_SECRETS_WRITE_TOKEN` is for); `llz` never asks for
them.

> **Manual alternative.** `llz secrets gather` (paste every credential yourself)
> + `llz secrets push <env> --yes` is still available if you'd rather not have
> the wizard create Linode resources for you.
>
> **Maintainers:** `llz tokens --admin` additionally wires the *template* repo's
> e2e harness (`E2E_INSTANCE_REPO` / `E2E_LINODE_REGION` / `E2E_OBJ_CLUSTER` +
> `E2E_DISPATCH_TOKEN`) and defaults the instance to the example repo. Adopters
> don't need it.

### Confirm readiness — `llz doctor`

```bash
llz doctor --env lab            # or: llz doctor --repo <owner>/<name> --env lab
```

The single **"am I ready to build?"** gate. In one run it checks all three things
that must be true before the build:

1. **Tooling + `gh` auth** — the CLIs the flow uses, and that `gh` is logged in.
2. **Deployment files** — when a spec is present, validates it and confirms the
   committed `apl-values` are in sync with it (so a spec edit you forgot to
   `llz render` is caught here); then scans the tfvars + overlay for residual
   placeholders, verifies the deployment discriminator agrees across the tfvars,
   and renders the overlay (the former `llz validate --env`).
3. **Repo config** — every variable/secret an e2e/build needs, required vs
   optional, set vs missing, merging your local `.llz/*.env` with the live repo
   config. (Variable *values* are read from the repo; secrets are presence-only —
   the same plan `llz tokens` prints.)

Green when every **required** item is set; otherwise it lists what's missing and
the command to fix it.

### Dispatch the apply — `llz build`

```bash
llz build lab --yes
```

Dispatches `terraform.yml` with `region=lab action=apply module=all`, which walks
the whole bootstrap end to end ([adopter-guide §6](adopter-guide.md#6-bootstrap-order)):

1. **Provision** the LKE-E cluster, VPC, firewall, node pool.
2. **Object storage** — registry/log buckets; OBJ keys auto-stashed into env secrets.
3. **Install apl-core** + apply the `apl-values/lab/manifest` Argo CD Applications.
4. **Converge** — polls until the cluster meets the [convergence contract](architecture/convergence-contract.md).
5. **Bootstrap OpenBao** (chained) — Raft init, unseal, KV v2, AppRole, seeds all
   platform secrets, populates GitHub secrets, revokes root.

</details>

---

## 5. Day-2 — upgrading to a newer upstream version

Two independent tracks, because the template ships two kinds of thing.

### Track A — the scaffold + first-party pins → `llz upgrade`

```bash
llz self-update                # get the new llz binary first (the version anchor)
llz upgrade                    # re-renders the scaffold + re-pins to llz's version
# or target a specific release explicitly:
llz upgrade --ref v0.2.0
```

Runs `copier update` (3-way merge — your local edits survive; conflicts appear as
`.rej`/merge markers only where you changed a line the template also changed),
then re-stamps `.template-version`. With no `--ref` it uses **this `llz` binary's
own version**, so the upgrade path is: `llz self-update` to the release you want,
then `llz upgrade`. Because the scaffold's first-party pins are rendered from
`llz_version`, the same `copier update` **re-pins the Terraform-module `?ref=`,
`uses:@`, and `template-ref:` refs in lockstep** — there is no separate version
bump for them. Ownership follows `.template-manifest`;
`terraform/*/.terraform.lock.hcl` files are seeded once and never re-touched.

Check how far behind you are any time:

```bash
llz drift           # compares .template-version against the template head
```

The **Scheduled Checks** workflow runs the same check monthly (its
`template-drift` job, 1st @ 07:00 UTC). Point it at the upstream with
`git remote add upstream <template-repo-url>`.

### Track B — independently-versioned artifacts → Renovate

The OCI chart `targetRevision`s and external GitHub Action digests version on
their own cadence and move via **Renovate PRs** (not `llz`).
`instance-template/renovate.json` ships in and bumps those. The first-party LLZ
module/workflow refs are **not** Renovate-managed — they ride `llz_version` and
move with `llz upgrade` (Track A), so Renovate is disabled on them to avoid
racing. After forking, repoint its `packageName` / `registryAliases` from
`akamai-consulting` to your fork. Details:
[adopter-guide §2](adopter-guide.md#keeping-the-pins-current--renovate).

**Rule of thumb:** `llz upgrade` moves the *scaffold and the first-party LLZ pins*
(in lockstep with the `llz` version); Renovate's PRs move the *independently-
versioned charts + external actions*.

---

## Checklist

- [ ] Accounts (§1): LKE-E, apl-core, GitHub org + instance repo
- [ ] `gh auth login` done (§2)
- [ ] `llz` installed + completion (§2); `llz doctor` tooling green
- [ ] `llz new … --push --yes` run; org literals repointed; instance pushed to GitHub (§3)
- [ ] `llz env add <env> --region … --obj-cluster …` run (authors `landingzone.yaml` + `environments/<env>.yaml`, renders); the overlay placeholders it listed are filled (§3)
- [ ] `llz doctor --env <env>` green — deployment files + every required value set (§4)
- [ ] `llz up <env> --yes` run (or `tokens → doctor → build`); cluster converges (`llz status <env>`) (§4)
- [ ] Unseal keys 4 & 5 + root token saved offline; `OPENBAO_ROOT_TOKEN` deleted
- [ ] `llz bootstrap dns <env> --yes` run once `LINODE_DNS_TOKEN` exists
- [ ] Renovate enabled and repointed; `llz upgrade` path understood (§5)

## See also

- [Dev Container](devcontainer.md) — open the instance in a ready-made workstation with the whole toolchain
- [Adopter guide](adopter-guide.md) — the same path with full rationale
- [Delivery methodology](delivery-methodology.md) — the phases this checklist walks, and how LLZ supports each
- [Linode account request checklist](infosec/linode-account-request-checklist.md) — account + InfoSec approval
- [OpenBao bootstrap runbook](runbooks/bootstrap-openbao.md) — full secret inventory + recovery modes
- [Secrets operations guide](secrets.md) — dual-write rotation, CI read path, failover
- [Operator onboarding](playbooks/operator-onboarding.md) — day-2 operations
