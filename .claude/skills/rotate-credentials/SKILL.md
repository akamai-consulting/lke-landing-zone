---
name: rotate-credentials
description: Guide Linode credential lifecycle operations - LKE admin kubeconfig rotation, PAT create/revoke, object-storage key create/revoke - through the llz credentials commands and their runbooks. User-invoked only - credential rotation is cloud-mutating.
disable-model-invocation: true
---

# Rotate Linode credentials

Read the matching runbook IN FULL before running anything — they are canonical;
this skill is the router:

| Credential | Runbook | Command |
|---|---|---|
| LKE admin kubeconfig (`lke-admin-token`) | `docs/runbooks/lke-admin-rotation.md` | `llz credentials lke-admin rotate` |
| Linode PATs | `docs/runbooks/linode-credential-rotation.md` | `llz credentials pat create` / `llz credentials pat revoke-old` |
| Object-storage keys | `docs/runbooks/linode-credential-rotation.md` | `llz credentials obj-key create` / `llz credentials obj-key revoke-old` |

Background on the secret architecture (dual-write HA OpenBao, CI read path,
failover): `docs/secrets.md`. Reconciler-driven rotation alerts:
`docs/runbooks/reconciler-alerts.md`.

## Hard rules

- **The `lke-admin-token` secret is rotated ONLY via the Linode
  delete-kubeconfig API** (which is what `llz credentials lke-admin rotate`
  drives). Never `kubectl delete` that secret — this is a documented
  hard-won lesson (`docs/lessons-learned.md`).
- Create-then-revoke, never revoke-first: the `create` / `revoke-old` split
  exists so the new credential is proven before the old one dies. Run
  `revoke-old` only after the new credential is verified in use.
- Every mutating command supports `--dry-run` — show the user the dry-run
  output and get explicit confirmation before running with `--yes`.
- In an HA pair, secrets are dual-written operator-side (no cross-region
  replication) — confirm which deployments are affected with
  `llz env list --ha` before rotating anything shared.
