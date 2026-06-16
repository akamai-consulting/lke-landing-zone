# Operator Onboarding — Playbook

**Applies to:** any team member receiving day-2 operations responsibility for your platform workloads on this landing zone.

**Related:** every other playbook in [`docs/playbooks/`](./) and runbook in [`docs/runbooks/`](../runbooks/). This is the doc that ties them together for a first-time operator.

---

## What you're getting access to

Five distinct surfaces, each with its own playbook for ongoing work:

| Surface | Where | Playbook | Auth model |
|---|---|---|---|
| Kubernetes (per cluster / per env) | LKE-Enterprise via `lke-admin` kubeconfig | — | kubeconfig (operator) |
| OpenBao (per region) | `<release>-openbao-0` pod in `openbao` ns | [openbao-accounts.md](openbao-accounts.md) | root via `generate-root` (3-of-5 unseal shares) |
| Harbor (primary only) | `harbor.<primary-cluster>.internal:5000` | [harbor-accounts.md](harbor-accounts.md) | admin password / per-person local user |
| Grafana (per region) | port-forward `<release>-grafana` in `observability` | [grafana-access.md](grafana-access.md) | admin / per-person local user |
| Loki (per region) | through Grafana or port-forward `<release>-loki-gateway` | [loki-access.md](loki-access.md) | `X-Scope-OrgID: <project>` header |

GitHub Actions (the CI surface) is separate: see [Git + GitHub access](#git--github-access) below.

---

## Onboarding checklist

Tick each item once you've successfully exercised the access. The whole checklist should take a few hours including the unseal-share handshake.

### 1. Local toolchain

```bash
# Clone
git clone git@github.com:<org>/<repo>.git
cd <repo>

# Enable the shared git hooks (pre-commit secret scan + audit, pre-push build check)
git config core.hooksPath template-scripts/hooks

# Install all required build + system tools (helm, kube-linter, syft, trivy, ...)
make install-tools

# Sanity-check
make build
```

If `make install-tools` fails on syft/trivy, see the install helpers under `template-scripts/ci/` — they auto-install on macOS via brew or via SHA-verified tarball on Linux.

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

Kubeconfigs are stored in Terraform state per cluster — CI fetches them via a composite action. Locally, do the same thing by hand:

```bash
export AWS_ACCESS_KEY_ID="$TF_STATE_ACCESS_KEY"           # from your password manager
export AWS_SECRET_ACCESS_KEY="$TF_STATE_SECRET_KEY"
export AWS_ENDPOINT_URL_S3="https://us-ord-10.linodeobjects.com"

cd instance-template/terraform-iac-bootstrap/cluster-bootstrap
for cluster in <cluster-1> <cluster-2>; do
  terraform init -reconfigure \
    -backend-config="bucket=<state-bucket>" \
    -backend-config="key=cluster/${cluster}/terraform.tfstate" \
    -backend-config="region=us-east-1"
  terraform output -raw kubeconfig_raw > ~/.kube/<project>-${cluster}.config
  chmod 0600 ~/.kube/<project>-${cluster}.config
done

# Then switch with KUBECONFIG=~/.kube/<project>-<cluster-1>.config kubectl get nodes
```

Verify:

- [ ] `kubectl get nodes` returns nodes on each cluster (skip envs that aren't deployed yet — `terraform output` will be empty).
- [ ] `kubectl -n openbao get pods` shows the 3-replica OpenBao StatefulSet.

### 4. OpenBao access

You don't get a permanent OpenBao token — operator access is via `bao operator generate-root` and requires 3-of-5 unseal-key shareholders to cooperate. The on-call docs list current shareholders; coordinate with them when you need a token.

Verify (no token needed for this — just port-forward):

- [ ] `llz openbao get <cluster> secret/<example-path> <key> | head -1` returns the expected first line. If it errors with auth, you don't have an operator token yet — see [openbao-accounts.md](openbao-accounts.md).

You'll receive an **unseal key share** if you're a shareholder. That share lives in your password manager forever and is required for re-unseal after pod restart — never lose it.

### 5. Harbor access

Harbor only runs on the primary cluster. Browse to `https://harbor.<primary-cluster>.internal:5000` (requires cluster network — VPN or jump host).

- [ ] You can log in with admin / password from [harbor-accounts.md](harbor-accounts.md#human-account--ui-login-recommended).
- [ ] An admin has created a per-person local-DB user for you with appropriate role on the `<project>` project (see [harbor-accounts.md](harbor-accounts.md)). After login, change your initial password.

### 6. Grafana access

- [ ] `kubectl -n observability port-forward svc/<release>-grafana 3000:80` works against the primary cluster.
- [ ] You can log in at <http://localhost:3000> as admin with the password from [grafana-access.md](grafana-access.md). An admin should create a per-person Grafana user for you.
- [ ] The platform dashboards load with data.

### 7. Loki access (sanity)

- [ ] In Grafana → Explore → Loki, run `{namespace="openbao"}` over the last 24h. You should see audit-log entries from OpenBao.

### 8. Argo CD access

Argo CD is the GitOps engine for everything under the Argo manifests directory (`instance-template/apl-values/example/manifest/`). To inspect deploys:

```bash
kubectl -n argocd port-forward svc/argocd-server 8080:443
# Browse to https://localhost:8080
# Get the initial admin password:
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d
```

- [ ] You can log in to Argo CD on the primary cluster.
- [ ] All Applications under the `<project>` project show `Healthy` + `Synced`.

If anything is out-of-sync or unhealthy, see [argocd-ops.md](argocd-ops.md).

### 9. Read the runbooks (don't memorize)

Skim each [`docs/runbooks/`](../runbooks/) file once so you know what exists and where. You'll come back to them when alerts fire:

- `lke-admin-rotation.md` — rotating LKE-Enterprise admin tokens (monthly)
- `approle-rotation.md` — rotating the ESO ClusterSecretStore AppRole (quarterly)
- `linode-credential-rotation.md` — Linode PAT + OBJ-key rotation
- `bootstrap-openbao.md` — first-time / re-bootstrap of OpenBao
- `orphan-volume-cleanup.md` — reclaiming orphaned block-storage volumes

### 10. On-call

- [ ] Added to the on-call rotation (PagerDuty / equivalent).
- [ ] Subscribed to the alerting destinations listed in [`docs/alerting.md`](../alerting.md).
- [ ] Know who else is on rotation; you'll need to reach a shareholder for break-glass OpenBao access.

---

## Things you cannot do as a fresh operator

Just so you don't burn time looking:

- **Delete OpenBao unseal keys / change the shareholder set** — requires a planned rekey ceremony with all current shareholders.
- **Push directly to `main`** — every change goes through PR + Argo CD; even rotation workflows are gated by GitHub Environment approval.
- **Change a `*.terraform.lock.hcl` provider version casually** — these come through dependabot PRs that the CVE gate audits.

---

## What "done" looks like

Onboarding is complete when you've ticked every box in the checklist above AND you've shadowed at least one of:

- A scheduled rotation run (monthly lke-admin or quarterly approle).
- A Grafana-dashboard-driven investigation of a real alert.
- An Argo CD sync of a non-trivial PR.

If any of those haven't happened in your first 30 days, ask to be paired into one — these are the muscle-memory operations that the playbooks alone can't teach.
