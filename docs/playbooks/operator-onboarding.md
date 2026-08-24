# Operator Onboarding — Playbook

**Applies to:** any team member receiving day-2 operations responsibility for your platform workloads on this landing zone.

**Related:** every other playbook in [`docs/playbooks/`](./) and runbook in [`docs/runbooks/`](../runbooks/). This is the doc that ties them together for a first-time operator.

---

<!-- toc -->
## Contents

- [What you're getting access to](#what-youre-getting-access-to)
- [Onboarding checklist](#onboarding-checklist)
- [Things you cannot do as a fresh operator](#things-you-cannot-do-as-a-fresh-operator)
- [What "done" looks like](#what-done-looks-like)
- [Next: run something on it](#next-run-something-on-it)

<!-- /toc -->

## What you're getting access to

Five distinct surfaces, each with its own playbook for ongoing work:

| Surface | Where | Playbook | Auth model |
|---|---|---|---|
| Kubernetes (per cluster / per env) | LKE-Enterprise via `lke-admin` kubeconfig | — | kubeconfig (operator) |
| OpenBao (per region) | `<release>-openbao-0` pod in `llz-openbao` ns | [openbao-team-login.md](../runbooks/openbao-team-login.md) → [openbao-accounts.md](openbao-accounts.md) | **per-person, via `llz openbao login --team`** (Keycloak device flow); root is break-glass only |
| Harbor (primary only) | the host the robot carries — `llz openbao get active secret/harbor/pull-robot registry_host` | [harbor-accounts.md](harbor-accounts.md) | admin password / per-person local user |
| Grafana (per region) | port-forward `<release>-grafana` in `grafana` | [grafana-access.md](grafana-access.md) | admin / per-person local user |
| Loki (per region) | through Grafana or port-forward `<release>-loki-gateway` in `monitoring` | [loki-access.md](loki-access.md) | `X-Scope-OrgID` header — tenant depends on the writer (`admins` for landing-zone namespaces) |

GitHub Actions (the CI surface) is separate: see [Git + GitHub access](#2-git--github-access) below.

---

## Onboarding checklist

Tick each item once you've successfully exercised the access. The whole checklist should take a few hours including the unseal-share handshake.

### 1. Local toolchain

Your instance repo carries the spec, the overlays, and the workflows — **not** a
Makefile or a scripts tree. Everything below is the `llz` binary plus the CLIs it
drives.

```bash
# Clone
git clone git@github.com:<org>/<repo>.git
cd <repo>

# Install llz (or `llz self-update` if you already have it)
curl -fsSL https://raw.githubusercontent.com/akamai-consulting/lke-landing-zone/main/template-scripts/install-llz.sh | bash

# Arm the pre-commit hook: secret-file guard + `llz lint`.
# The hook is per-clone and NOT committed, so this is required after every fresh clone.
llz hooks

# Sanity-check the toolchain (authoritative, always-current list of what the flow needs)
llz doctor
```

> **Do not set `core.hooksPath`.** `llz hooks` installs to `.git/hooks/pre-commit`.
> Pointing `core.hooksPath` somewhere else makes git ignore that directory
> entirely — and if the path does not exist, **no hooks run at all**, silently,
> including the secret guard.

`llz doctor` lists anything missing and the command to fix it. To skip host
installs altogether, open the repo in its [Dev Container](../devcontainer.md) —
same toolchain, prebuilt.

Add your own extra pre-commit checks in `.githooks/pre-commit.local` (an `owned`
file the template never touches); `llz precommit` runs it after the built-in gate.

### 2. Git + GitHub access

- [ ] Repo membership on `github.com/<org>/<repo>` with at least `write` for routine PRs (or `read` if you're audit-only).
- [ ] SSH key uploaded to the git host.
- [ ] A personal PAT for any CLI use you need — store in your password manager, never in repo.
- [ ] A separate **github.com PAT** with `read:packages` scope for GHCR pulls (`ghcr.io` is github.com, not your git host). Run once:

    ```bash
    echo "$YOUR_GH_COM_PAT" | crane auth login ghcr.io -u <github-username> --password-stdin
    ```

    Writes creds to `~/.docker/config.json`; trivy / syft / crane all read it. Required to scan any of the GHCR-hosted images locally.

### 3. Kubernetes access (per cluster)

Kubeconfigs are stored in Terraform state per cluster. **Do not drive Terraform by
hand for this** — every root carries `encryption.tf`, so a `terraform init`/`output`
without `TF_ENCRYPTION` fails with OpenTofu's own unhelpful message (*"Invalid
expression … A single static variable reference is required"*). Two supported ways:

```bash
export LINODE_API_TOKEN=…        # both paths need it; A also needs the TF state creds

# A. From Terraform state — what CI uses. Run from the cluster root; the command
#    handles init, the encryption env, and the empty-output diagnostics.
cd terraform-iac-bootstrap/cluster
llz ci fetch-kubeconfig-state --region <env> --output ~/.kube/<instance>-<env>.config

# B. From the Linode API — no terraform, no S3 backend, no state passphrase.
#    Prefer this when you are locked out and unsure what still works.
llz ci fetch-kubeconfig --region <env> --output ~/.kube/<instance>-<env>.config
```

> **In a fresh clone, give B the cluster explicitly.** `--region` resolves the
> cluster by reading `cluster_label`/`region` out of
> `terraform-iac-bootstrap/cluster/<env>.tfvars` — and those tfvars are
> **gitignored build artifacts**, so a clone that has not run `llz render <env>`
> does not have them. Either render first, or name the cluster yourself, which
> needs nothing from the repo at all:
>
> ```bash
> llz ci fetch-kubeconfig --cluster-label <label> --linode-region <us-ord> \
>   --output ~/.kube/<instance>-<env>.config
> # or, if you have the numeric id:  --cluster-id 638034
> ```

Both write mode `0600`. Then:

```bash
export KUBECONFIG=~/.kube/<instance>-<env>.config
kubectl get nodes
```

> **The control-plane ACL may refuse you.** LKE-E clusters restrict API-server
> access to the CIDRs the spec seeded plus whatever the in-cluster firewall
> controller reconciles. A `kubectl` that hangs or times out from a new location is
> usually the ACL, not the kubeconfig — get your egress CIDR added to
> `cluster.apiServerAllowCIDRs`.

Verify:

- [ ] `kubectl get nodes` returns nodes on each cluster (skip envs that aren't deployed yet — the fetch reports `available=false`).
- [ ] `kubectl -n llz-openbao get pods` shows the 3-replica OpenBao StatefulSet.

### 4. OpenBao access

You get a **per-person, team-scoped** login through your APL/Keycloak identity — not
a permanent token and not a root token. A platform admin adds you to the
`team-<name>` group; then:

```bash
eval "$(llz openbao login --team <name>)"     # browser device flow → OPENBAO_TOKEN
```

- [ ] A platform admin has added you to a `team-<name>` group (`llz apl user add --email <you> --team <name> --yes`).
- [ ] `llz openbao login --team <name>` returns a token, and `llz openbao get active secret/<your-team>/<path> <key> | head -1` reads back. Full flow + troubleshooting: [openbao-team-login.md](../runbooks/openbao-team-login.md).

Root is break-glass only, and it is revoked at the end of every bootstrap — the
supported way to get one back is the `breakglass-openbao.yml` workflow, which
reconstitutes it from the recovery quorum stored in the `infra-<env>` environment.
You do **not** need to hold recovery keys for that.

If you *are* a recovery-key shareholder you'll receive a share. It lives in your
password manager forever — it authorizes `generate-root` (emergency root-token
regeneration), not unseal: the cluster auto-unseals itself from a static seal key
after a pod restart. Never lose it.

### 5. Harbor access

Harbor runs on the primary cluster. **Don't hand-assemble the URL** — on Managed App
Platform the domain is Linode's (`lke<id>.akamai-apl.net`), so read the authoritative
host from the robot credential:

```bash
llz openbao get active secret/harbor/pull-robot registry_host    # → the host to browse
```

Reaching it needs cluster network (VPN or jump host).

- [ ] You can log in with admin / password from [harbor-accounts.md](harbor-accounts.md#human-account--ui-login-recommended).
- [ ] An admin has created a per-person local-DB user for you with the appropriate role on the `platform` project (see [harbor-accounts.md](harbor-accounts.md)). After login, change your initial password.

### 6. Grafana access

- [ ] `kubectl -n grafana port-forward svc/<release>-grafana 3000:80` works against the primary cluster.
- [ ] You can log in at <http://localhost:3000> as admin with the password from [grafana-access.md](grafana-access.md). An admin should create a per-person Grafana user for you.
- [ ] The platform dashboards load with data.

### 7. Loki access (sanity)

- [ ] In Grafana → Explore → Loki, run `{namespace="llz-openbao"}` over the last 24h.

If that returns nothing, check the data source's tenant before assuming the pipeline
is broken: Loki runs `auth_enabled: true` here, the OpenBao audit stream is written
under tenant `platform`, and everything else in a landing-zone namespace lands under
`admins`. An empty result is the signature of the wrong tenant, not a dead writer —
see [loki-access.md](loki-access.md).

### 8. Argo CD access

Argo CD is installed and owned by apl-core, and it reconciles the LLZ Applications
your instance's `apl-values/<env>/manifest` overlay points at.

Log in through the platform console's Argo CD link (Keycloak SSO — the same identity
as §4), or port-forward for a direct look:

```bash
kubectl -n argocd port-forward svc/argocd-server 8080:443
# Browse to https://localhost:8080 and use the SSO button.
```

> The upstream chart's `argocd-initial-admin-secret` local admin is **not** the
> intended path here and may not exist on an apl-core-managed Argo CD. If you need a
> local admin for a break-glass case, check whether the Secret is present before
> planning around it:
> `kubectl -n argocd get secret argocd-initial-admin-secret`.

- [ ] You can log in to Argo CD on the primary cluster.
- [ ] Every LLZ Application shows `Healthy` + `Synced` (`llz status <env>` reports the same thing without a browser).

If anything is out-of-sync or unhealthy, see [argocd-ops.md](argocd-ops.md).

### 9. Read the runbooks (don't memorize)

Skim each [`docs/runbooks/`](../runbooks/) file once so you know what exists and where. You'll come back to them when alerts fire:

- `bootstrap-openbao.md` — first-time / re-bootstrap of OpenBao, **and the break-glass root-token workflow**
- `openbao-team-login.md` — onboarding a person to a team; the whole `login --team` troubleshooting matrix
- `lke-admin-rotation.md` — rotating LKE-Enterprise admin tokens (monthly)
- `linode-credential-rotation.md` — Linode PAT + OBJ-key rotation
- `orphan-volume-cleanup.md` — reclaiming orphaned block-storage volumes
- `volume-labels.md` — why a Volume is named what it is (and what that means for cleanup)
- `reconciler-alerts.md` — triaging `LLZReconciler*` alerts
- `apl-values-propagation.md` — a values change that hasn't reached the cluster
- `apl-branch-recreate-wedge.md` — the `apl-<env>` branch wedge
- `import-apl-site.md` — adopting an existing APL site onto LLZ
- `e2e-lane-diagnostics.md` — debugging the release-e2e lane (maintainers)

### 10. On-call

- [ ] Added to the on-call rotation (PagerDuty / equivalent).
- [ ] Subscribed to the alerting destinations listed in [`docs/alerting.md`](../alerting.md).
- [ ] Know who else is on rotation; you'll need to reach a shareholder for break-glass OpenBao access.

---

## Things you cannot do as a fresh operator

Just so you don't burn time looking:

- **Delete OpenBao recovery keys / change the shareholder set** — requires a planned rekey ceremony with all current shareholders.
- **Push directly to `main`** — every change goes through PR + Argo CD; even rotation workflows are gated by GitHub Environment approval.
- **Change a `*.terraform.lock.hcl` provider version casually** — nothing bumps these for you. Your instance ships no Dependabot
  config, and the lock file is yours (`owned` in `.template-manifest`), so `llz upgrade` never touches it either. A provider move
  is a deliberate `tofu init -upgrade` in its own PR, with the resulting plan read before merge.

---

## What "done" looks like

Onboarding is complete when you've ticked every box in the checklist above AND you've shadowed at least one of:

- A scheduled rotation run (monthly lke-admin or Linode PAT rotation).
- A Grafana-dashboard-driven investigation of a real alert.
- An Argo CD sync of a non-trivial PR.

If any of those haven't happened in your first 30 days, ask to be paired into one — these are the muscle-memory operations that the playbooks alone can't teach.

---

## Next: run something on it

This checklist gets you *access to* the platform. Putting your own application on it
is a different set of contracts — which Harbor project, why nothing hands your
namespace an imagePullSecret, where an app's secrets have to live for External
Secrets to read them. Those are walked end to end in
[**Run your first workload**](first-workload.md).
