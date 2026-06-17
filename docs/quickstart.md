# Quick Start

> **Goal:** go from nothing to a converging LKE-Enterprise + apl-core cluster
> built from this template, driving the whole flow with the **`llz`** CLI.
>
> **Audience:** a system team standing up its own instance. You're expected to
> already be on the stack (Linode LKE-E + Akamai App Platform). For the full
> rationale behind each step, read the [adopter guide](adopter-guide.md); this
> page is the fast path.

## The golden path — four commands

After the [accounts](#1-accounts-you-need) exist, the whole flow is four commands
from a checkout:

```bash
./template-scripts/install-llz.sh                          # 1. install the llz CLI (§2)
llz new my-instance --org <org> --push --yes               # 2. scaffold + create/push the instance repo (§3)
cd my-instance && llz env add lab --region us-sea --obj-cluster us-sea-1   # 3. add a deployment, then fill the checklist it prints (§3)
llz up lab --yes                                           # 4. credentials → readiness gate → build, in one go (§4)
```

`llz` (a [cobra](https://github.com/spf13/cobra)-based CLI) is a thin front-end
over the tools this flow already uses (`copier`, `gh`, `kubectl`, and the Linode
API). It doesn't replace them — it sequences them and adds a **provisioning
wizard** (`llz tokens`) that *creates* the Terraform-state bucket + a scoped key,
*generates* the ArgoCD deploy key, gathers the GitHub PATs behind pre-filled
links, and pushes everything to your repo. `llz up` chains the last three steps
(`tokens → doctor → build`); the sections below cover each command, and you can
always run them individually.

Run `llz <command> --help` for any command; the persistent flags `--dry-run`
(print, change nothing), `--open` (open links), and `--yes` (execute
cloud-mutating commands) work anywhere on the line.

---

## 1. Accounts you need

`llz` can't create these — get them first. The full table (the *why* + where to
get each) is canonical in the [adopter guide §1](adopter-guide.md#1-prerequisites);
the short version:

- **Linode account with LKE-Enterprise** — `+lke` versions, not standard LKE
- **Akamai App Platform (apl-core) entitlement**
- **A GitHub org + an instance repo** — a fork of the template org, or your own
- **A GitOps repo reachable over HTTPS** — github.com, gitlab.com, or an internal HTTPS mirror (often the same repo)
- **GHCR pull access** — Argo CD pulls the first-party charts from `ghcr.io/<org>/charts`; these are **public**, so it pulls them anonymously — no credential needed

> **Start the Linode account first — it has the longest lead time.** Production
> accounts need an executive sponsor + InfoSec approval: follow the
> [Linode account request checklist](infosec/linode-account-request-checklist.md).

Run **`llz doctor`** any time to check your CLI tooling + `gh` auth — it is the
authoritative, always-current list of what the flow needs. With a repo/env it
also reports deployment + e2e readiness (see §4).

---

## 2. Install `llz`

The template repo is **public**, so the download needs no auth. The install
script uses `gh` (already a prerequisite; see `llz doctor`), which also works
against a private fork or a GHE host. From a template or instance
checkout, the install script picks your platform, resolves the latest full
release, verifies the checksum, and installs to **`~/.local/bin`** — a per-user
dir that needs no `sudo` and works on locked-down corporate machines that deny
writes to `/usr/local/bin`:

```bash
./template-scripts/install-llz.sh            # latest release; ORG=<fork> to use your fork
# or pin a tag:  ./template-scripts/install-llz.sh v0.2.0
llz version
```

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
llz new my-instance --org akamai-consulting --push --yes
cd my-instance
llz env add lab --region us-sea --obj-cluster us-sea-1
```

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

`llz env add` generates
`terraform-iac-bootstrap/{cluster,cluster-bootstrap,object-storage}/lab.tfvars`
and the `apl-values/lab/` overlay, then **prints an exact checklist of the
placeholders you still need to fill** (file:line + what each is). Several
must-set values are accepted as **flags**, so you can set them up front instead
of hand-editing:

```bash
llz env add lab --region us-sea --obj-cluster us-sea-1 \
  --k8s-version v1.33.6+lke7 --node-type g8-dedicated-8-4 --node-count 5 \
  --runner-ipv4-cidrs 203.0.113.0/24
```

The **ADOPTER-MUST-SET** values (full table in
[adopter-guide §3](adopter-guide.md#3-the-values-contract-what-you-must-set)):

- `region`, `k8s_version` (an LKE-E `+lke` version in your account), node sizing (`--node-type`/`--node-count`)
- `github_runner_ipv4_cidrs` / `*_ipv6_cidrs` — static operator/CI egress CIDRs that seed the bootstrap control-plane ACL (**never `0.0.0.0/0`**; leave empty for github.com-hosted runners, which open their egress IP at runtime via `llz ci runner-acl open`)
- `cluster_name`, `cluster_domain`, `apl_values_repo_url` (**HTTPS**, defaults from `instance_repo`), `apl_chart_version`
- `obj_cluster` — your region's Linode OBJ cluster id (e.g. `us-ord-1`, or a newer-generation `us-ord-10`). List them with `linode-cli object-storage clusters-list`; `llz env add` validates the shape up front.

Fill the placeholders `env add` listed, then confirm readiness:

```bash
llz doctor --env lab   # scans tfvars + overlay for residual placeholders, renders the overlay
```

`llz doctor --env` is the single readiness gate (full breakdown in §4). Run it
now for the local file checks — the repo-config part fills in once `llz tokens`
has pushed. Or run `make instance-test` for a fast, no-cloud smoke test of the
whole instantiation path before paying for a real build.

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
                      # moment its cluster/<name>.tfvars exists.
llz env list --ha     # only deployments in an OpenBao HA pair (ha_role != standalone)
llz env role lab      # active | standby | standalone (from cluster/lab.tfvars)
llz env peer lab      # the deployment paired with lab (errors if standalone)
```

Most deployments are **standalone** (a single self-contained OpenBao — the
`llz env add` default). For a two-cluster HA pair, scaffold both with a shared
`--ha-group` and opposite roles:

```bash
llz env add east --region us-sea --obj-cluster us-sea-1 --ha-role active  --ha-group prod
llz env add west --region us-ord --obj-cluster us-ord-1 --ha-role standby --ha-group prod
```

The bootstrap, rotation, and Harbor workflows resolve `ha_role`/peer from the
tfvars instead of hardcoding which cluster is which.

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

> ⚠️ **After the run, do the two manual steps the bootstrap can't:** copy
> **unseal keys 4 & 5 and the root token** from the job summary to secure offline
> storage (shown once), and delete `OPENBAO_ROOT_TOKEN` from `infra-lab` if you
> set it (`llz status` flags it on every run until you do). See the
> [bootstrap runbook](runbooks/bootstrap-openbao.md#after-first-time-bootstrap--required-operator-actions).

Then finish the deferred DNS bit once its token exists (the ArgoCD deploy key was
already provisioned by `llz tokens`; `llz bootstrap ssh` is for *rotating* it
later), and verify convergence:

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
2. **Deployment files** — scans the tfvars + overlay for residual scaffold
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

- [ ] Accounts (§1): LKE-E, apl-core, instance repo, HTTPS GitOps repo, inventory repo
- [ ] `llz` installed + completion (§2); `llz doctor` tooling green
- [ ] `llz new … --push --yes` run; org literals repointed; instance pushed to GitHub (§3)
- [ ] `llz env add <env>` run; the placeholders it listed are filled (`obj_cluster` set to your region's OBJ cluster id) (§3)
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
