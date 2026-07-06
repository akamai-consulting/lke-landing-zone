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
in-cluster Linode PAT. Moving the **broad Linode CI/TF PAT rotation** in-cluster —
mint → seed OpenBao → write the new value back to the GitHub Actions secret (nacl
sealed box) → revoke old — is a follow-up (it's security-sensitive: the cluster gains
an `account:read_write` token). GitHub PATs cannot be API-rotated and stay human-rotated
— the lead-time alerts above ensure that never lapses silently.

## Code touch-points

`cmd/llz/ci_token_inventory.go` (writer) · `cmd/llz/reconcile_tokens.go` (reader) +
`--reconcile-token-inventory` · `components/llzReconciler/…/rbac.yaml` (ConfigMap get) ·
`components/observability/{cert-manager-servicemonitor,cert-manager-allow-metrics,
credential-inventory-dashboard,prometheus-rules/credential-alerts}.yaml` ·
`llz-scheduled-checks.yml` (`credential-single-pane` job).
