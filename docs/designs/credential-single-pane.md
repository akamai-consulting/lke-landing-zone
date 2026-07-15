# Credential single pane of glass

One Prometheus/Grafana view of every credential the platform depends on — CI tokens
**and** certificates — with alerts that fire **before** anything expires. Replaces the
per-provider scheduled probe jobs (`gh-pat-expiry-health`, `linode-pat-expiry-health`)
with a cluster-as-source-of-truth flow. Supersedes PR #107.

## The problem

Credential status was scattered: cert-manager renewal was only caught once a cert was
already `Ready=False` (the reconciler's `LLZCertificatesNotReady`), and external CI
tokens (GitHub / Linode PATs) were invisible to Prometheus — each had its own CI job
that re-probed and went red. No single pane, and no *lead-time* on token expiry.

## Data flow

```
                external tokens live OUTSIDE the cluster
                            │
 scheduled job:  llz ci token-inventory   (holds the tokens; measures expiry)
                            │  writes (metadata only — never a token value)
                 ConfigMap llz-reconciler/llz-token-inventory
                            │  read every 60s
 in-cluster:     llz reconcile --reconcile-token-inventory
                            │  re-exposes
                 llz_token_expiry_timestamp_seconds{provider,token}
                 llz_token_audit_ok{provider,token}
                 llz_token_inventory_updated_timestamp_seconds
                            │  scraped by apl-core Prometheus
 certs:          cert-manager :9402  ── ServiceMonitor + allow-netpol ──┘
                            │
                 PrometheusRule credential-alerts  (fire BEFORE expiry)
                 Grafana "LLZ Credentials — single pane"
                            │
 scheduled job:  llz ci alert-eval --match '^LLZ(Token|Certificate|Credential)' --strict
                 (the cluster is the status source; --strict catches a dead funnel)
```

Only a job that *holds* a token can measure its expiry, so `token-inventory` is the one
writer; everything downstream reads the cluster. The ConfigMap carries `{provider, name,
expiry, state}` only — **no token material**.

## Alerts (the lead-time half)

- `LLZTokenExpiringSoon` (<14d) / `LLZTokenExpiringCritical` (<3d)
- `LLZTokenAuditBreach` — no-expiry / expired / over-90d / invalid (`audit_ok == 0`)
- `LLZTokenInventoryStale` — the funnel stopped writing (absent or >26h old)
- `LLZCertificateExpiringSoon` (<7d) / `LLZCertificateExpiringCritical` (<48h)

These complement the reconciler's existing `LLZCredentialRotationOverdue` (object-store
key age >90d) and `LLZCertificatesNotReady` (already-broken).

## Rotation

The cluster already rotates object-storage keys (`linodeCredRotator`) and the narrow
in-cluster Linode PAT. The **broad Linode CI/TF PAT rotation** is moved in-cluster by
the `broadPatRotator` component (`llz ci rotate-broad-pat`) — a dedicated, weekly
CronJob, deliberately NOT the always-on reconciler (which holds only the narrow PAT).
When the OpenBao `rotated_at` is older than `ROTATE_AFTER_DAYS`, in this order:

1. mint a fresh broad PAT (`account:read_write`, 90d) with the current token;
2. **verify** it (`GET /v4/profile`) — a bad mint drains nothing;
3. write it to OpenBao (`secret/linode/broad-pat`) — ESO refreshes the CronJob's own
   token for the next run;
4. publish it to each deployment's `infra-<d>` GitHub **environment** secret
   `LINODE_API_TOKEN` (`ghSetEnvSecretNative`, libsodium sealed box);
5. only now revoke older same-labeled PATs **outside the grace window**, keeping the
   newest — the token CI is actively using is never pulled out from under it.

A partial publish is safe (the old token stays valid until the grace-windowed revoke)
and aborts the run before revoke, so it retries next cadence. **Security:** this is the
one in-cluster workload holding an `account:read_write` Linode token + a GitHub
env-secrets-write token (both ESO-synced, scoped to its own SA + OpenBao role
`broad-pat-rotator`), isolated in the `llz-pat-rotator` namespace. It is
**default-disabled** and — because the broad PAT is account-wide — must be enabled on
**exactly one** deployment.

**Activation** (one-time): seed the current `LINODE_API_TOKEN` value into OpenBao at
`secret/linode/broad-pat`, set the CronJob's `BROAD_PAT_LABEL` (the CI PAT family label)
+ `BROAD_PAT_DEPLOYMENTS` (all deployment names), and enable `spec.components.broadPatRotator`
on one deployment. GitHub PATs cannot be API-rotated and stay human-rotated — the
lead-time alerts above ensure that never lapses silently.

## Code touch-points

`cmd/llz/ci_token_inventory.go` (writer) · `cmd/llz/reconcile_tokens.go` (reader) +
`--reconcile-token-inventory` · `components/llzReconciler/…/rbac.yaml` (ConfigMap get) ·
`components/observability/{cert-manager-servicemonitor,cert-manager-allow-metrics,
credential-inventory-dashboard,prometheus-rules/credential-alerts}.yaml` ·
`llz-scheduled-checks.yml` (`credential-single-pane` job).
