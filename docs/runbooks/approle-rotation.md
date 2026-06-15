# OpenBao AppRole Credential Rotation — Runbook

**Applies to:** the CI AppRole (`<instance>-ci`) used by ESO ClusterSecretStore to authenticate to OpenBao (each regional cluster)  
**Policy:** your org's secret-rotation policy — LKE secret rotation

---

## Why this process exists

The **ESO ClusterSecretStore** (`openbao` on each regional cluster) authenticates to OpenBao using AppRole credentials: a `role_id` (static, non-sensitive) and a `secret_id` (sensitive, functionally equivalent to a password). ESO uses these to pull secrets from OpenBao into Kubernetes Secrets for all on-cluster platform workloads (Grafana, Loki, Harbor, the AppRole rotation CronWorkflow, your application workloads, etc.). CI/CD workflows do not authenticate to OpenBao directly — they read runtime secrets from GitHub Actions environment secrets.

The `secret_id` must be rotated periodically to limit blast radius if it is ever exposed via a log leak, secret exfiltration, or similar. OpenBao issues short-lived service tokens from AppRole logins (`token_ttl=15m`, `token_max_ttl=30m`), but the `secret_id` used for that login itself has no automatic expiry unless a `secret_id_ttl` is configured on the role. This runbook enforces rotation on a 90-day schedule via a CronWorkflow.

The `OPENBAO_APPROLE_ROLE_ID` / `OPENBAO_APPROLE_SECRET_ID[_STANDBY]` GitHub Actions secrets mirror the in-cluster `eso-approle-secret` K8s Secret so that a full re-bootstrap can restore ESO auth without starting from scratch.

---

## Automated path (target state)

The rotation should be fully automated and **in-cluster** using Argo Workflows. The workflow authenticates to OpenBao via Kubernetes auth (ServiceAccount JWT), so it never needs the existing `secret_id` to generate a new one.

```
CronWorkflow fires every 90 days in each cluster (quarterly: Jan 1, Apr 1, Jul 1, Oct 1 at 02:00 UTC)
        │
        ▼  Argo Workflow (regional OpenBao cluster)
  ├─ bao auth kubernetes login (SA JWT) → short-lived token
  ├─ bao write auth/approle/role/<instance>-ci/secret-id → new regional secret_id
  ├─ patch eso-approle-secret in the local cluster
  ├─ gh secret set OPENBAO_APPROLE_SECRET_ID[_STANDBY] (GitHub API)
  ├─ Verify: test AppRole login with new secret_id works
  └─ Revoke old secret_id accessors after GitHub secrets are updated
```

**Prerequisites for the automated path:**
- OpenBao Kubernetes auth method enabled on every cluster (`bao auth enable kubernetes`)
- `approle-rotator` ServiceAccount bound to an OpenBao Kubernetes role with `auth/approle/role/<instance>-ci/secret-id` write capability
- `approle-rotation-secrets` K8s Secret in the `openbao` namespace containing `github_token` (PAT, `secrets:write` scope) — synced from OpenBao via ESO

✅ **Automated path is live.** The CronWorkflow is deployed via the `<release>-openbao` Argo CD Application in each cluster. Which GitHub secret it updates is derived from the deployment's `ha_role` (set via the chart's `approleWorkflow.role`): an **active** or **standalone** cluster updates `OPENBAO_APPROLE_SECRET_ID`; a **standby** updates `OPENBAO_APPROLE_SECRET_ID_STANDBY`. Use the manual path below only for emergency rotation or if the CronWorkflow fails.

---

## Manual operator path (interim fallback)

**Rotation trigger:** Calendar reminder every 90 days, OR immediately if a `secret_id` is suspected compromised.

**Required access:**
- `kubectl` access to each LKE cluster (your approved access path — e.g. TCP proxy or corporate VPN)
- `gh` CLI authenticated to your git host with `secrets:write` on the `<org>/<instance>` repository

---

## Step 1 — Generate new secret-id on the primary cluster

```bash
# Port-forward to the primary OpenBao
kubectl port-forward -n openbao svc/openbao 8200:8200 &
export VAULT_ADDR=https://127.0.0.1:8200
export VAULT_SKIP_VERIFY=true   # TLS cert is self-signed; skip verification over port-forward

# Authenticate as an operator (use your personal OpenBao token or admin AppRole)
export VAULT_TOKEN=<your-operator-token>

# Capture the new secret_id into a shell variable — needed for Step 2 verification.
# WARNING: do not echo, print, or log this value — it is a live credential.
NEW_SECRET_ID_PRIMARY=$(bao write -field=secret_id auth/approle/role/<instance>-ci/secret-id)
```

---

## Step 2 — Verify the new secret-id authenticates

```bash
ROLE_ID=$(bao read -field=role_id auth/approle/role/<instance>-ci/role-id)

# Test login with new credentials — should return a token
bao write auth/approle/login \
  role_id="$ROLE_ID" \
  secret_id="$NEW_SECRET_ID_PRIMARY" \
  | grep token
```

If this fails, do not proceed — investigate before updating GitHub Actions.

---

## Step 3 — Repeat on the secondary cluster

```bash
# Kill primary port-forward, forward to secondary cluster
kill %1
kubectl --context=<secondary-context> port-forward -n openbao svc/openbao 8200:8200 &
export VAULT_ADDR=https://127.0.0.1:8200
export VAULT_SKIP_VERIFY=true

# Authenticate to secondary (separate token / auth)
export VAULT_TOKEN=<secondary-operator-token>

# Capture the new secret_id for the secondary cluster.
# WARNING: do not echo, print, or log this value — it is a live credential.
NEW_SECRET_ID_SECONDARY=$(bao write -field=secret_id auth/approle/role/<instance>-ci/secret-id)
```

---

## Step 4 — Push new secret-ids to GitHub and clear shell variables

```bash
printf '%s' "$NEW_SECRET_ID_PRIMARY" \
  | gh secret set OPENBAO_APPROLE_SECRET_ID --repo <org>/<instance>

printf '%s' "$NEW_SECRET_ID_SECONDARY" \
  | gh secret set OPENBAO_APPROLE_SECRET_ID_STANDBY --repo <org>/<instance>

unset NEW_SECRET_ID_PRIMARY NEW_SECRET_ID_SECONDARY
```

Verify the secrets were updated in the repository Settings → Secrets and variables → Actions.

---

## Step 5 — Validate via ESO ClusterSecretStore

The AppRole credentials are used by the ESO ClusterSecretStore on each cluster (not by CI deploy workflows). After rotation, verify ESO can still authenticate:

```bash
# Check that the ClusterSecretStore status is still Ready on every cluster
kubectl get clustersecretstore openbao -o wide
# STATUS should be "Valid"

# Verify a sample ExternalSecret has synced a recent timestamp
kubectl -n observability get externalsecret grafana-admin-credentials
```

**If the ClusterSecretStore shows an auth error:**

- **Automated path (CronWorkflow):** Check that the `eso-approle-secret` K8s Secret in the `external-secrets` namespace was patched by the CronWorkflow's rotate-primary step. If not, re-run the CronWorkflow.
- **Manual path (Steps 1–4):** Steps 1–4 update the GitHub secrets only. The `eso-approle-secret` K8s Secret still holds the old `secret_id` and must be patched manually — see the rotate step in `instance-template/apl-values/example/manifest/openbao/argocd/applications/templates/approle-rotation-cronworkflow.yaml` for the exact patch operation, or re-run `bootstrap-openbao.yml` in re-configure mode to fully re-seed credentials.

---

## Step 6 — Revoke old secret-ids (optional but recommended)

If the previous `secret_id` values are known (e.g., stored securely during the previous rotation), revoke them:

```bash
# Primary
bao write auth/approle/role/<instance>-ci/secret-id-accessor/destroy \
  secret_id_accessor=<previous-accessor>

# Secondary (repeat)
```

If the previous accessors are not known, the old `secret_id` values will remain valid until they age out (if `secret_id_ttl` is configured) or until the role is modified to invalidate all existing `secret_id`s:

```bash
# Nuclear option — invalidates ALL secret_ids for the role
bao write auth/approle/role/<instance>-ci/secret-id-accessor/destroy-all
```

---

## Rollback

If the updated GitHub Actions secrets cause CI failures, re-generate another `secret_id` pair (Steps 1–4) immediately. There is no way to restore a previous `secret_id` value — always generate a fresh one.

---

## Unseal key rotation

OpenBao unseal keys are distinct from AppRole credentials and require a quorum of key holders. Unseal key rotation is **not automated** and is performed only during planned maintenance:

```bash
# Requires threshold-of-N key holders (e.g., 3-of-5)
bao operator rekey -init -key-shares=5 -key-threshold=3
# Distribute key shares to new holders
# Complete rekey with bao operator rekey -nonce=... <key-share>
```

Unseal key rotation should be performed:
- Annually (planned maintenance window)
- When a key holder leaves the platform team
- When an unseal key is suspected compromised

---

## Security notes

| Risk | Mitigation |
|------|-----------|
| `secret_id` values visible in terminal history | Captured in a shell variable (not echoed); piped via `printf '%s'` to `gh secret set`; cleared with `unset` in Step 4 immediately after upload |
| Port-forward exposes OpenBao locally | Kill port-forward immediately after rotation (Step 5); do not leave running |
| Gap between old and new `secret_id` during update | Step 2 validates the new credential before removing the old one — no gap |
| Rotation fails mid-way (primary updated, secondary not) | Retry Step 3 with a fresh `secret_id`; OpenBao generates a new one each call |
| CI job picks up old `secret_id` during rotation window | GitHub Actions reads secrets at job start; in-flight jobs complete with old creds; new jobs use new creds |
