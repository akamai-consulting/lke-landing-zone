---
name: triage-e2e
description: Triage a failed release-e2e run - map the failure to a known wedge class, find the failing step in the workflow logs, and clean up leaked Linode resources (orphan clusters, NodeBalancers, VPCs, volumes). Use when release-e2e fails, a bootstrap wedges, an e2e cluster leaks resources, or a fresh cluster-create hangs.
---

# Triage a release-e2e failure

`release-e2e.yml` stands up a REAL LKE-Enterprise cluster (instantiate →
provision → validate → destroy) — slow and billable, so triage before rerunning.

## 1. Get the failure

```bash
gh run list --workflow=release-e2e.yml --limit 5
gh run view <run-id> --log-failed
```

## 2. Map it to a known wedge class first

Each class below already has a PR-time guard — if one of these recurs, the
guard has a gap; fix the guard (see the `add-ci-guard` skill), not just the
symptom. The Makefile comment above each target documents the original wedge:

| Symptom | Class | Guard |
|---|---|---|
| platform-bootstrap sync stuck before OpenBao (wave 0) | negative-wave kind not health-inert (PR #142) | `wave-health-guard` |
| workload never Healthy, later-wave ExternalSecrets starved | workload waves before the ExternalSecret it needs (#163) | `wave-dependency-guard` |
| cross-namespace traffic to harbor silently dropped | egress into an Istio STRICT-mesh namespace | `mesh-egress-guard` |
| metrics unscraped / alerts never fire, everything else green | monitor/rule CR missing `prometheus: system` (#175) | `monitoring-label-guard` |
| Argo 404s a chart version on cold bootstrap; support-plane stranded | pin never published | `chart-pin-guard` / `chart-version-guard` |
| ArgoCD Applications deadlocked on greenfield install | missing sync-wave annotation | `sync-wave-lint` |

Also check `docs/runbooks/apl-branch-recreate-wedge.md` for the apl-values
branch wedge, and `docs/architecture/convergence-contract.md` for what
"converged" is supposed to mean.

## 3. Clean up leaked resources

Failed/cancelled cycles leak Linode resources, and the backlog makes the NEXT
cluster-create HANG — clean up before rerunning:

```bash
# DRY-RUN by default; needs LINODE_TOKEN; REGION recommended
make reap-orphans REGION=<region> CLUSTER_LABEL=<label>
# then, after reviewing the dry-run output, with the user's explicit go-ahead:
make reap-orphans REGION=<region> CLUSTER_LABEL=<label> CONFIRM=yes
```

Sweeps in dependency order: clusters (if CLUSTER_LABEL) → firewall/VPC →
NodeBalancers → VPCs → Volumes whose cluster is gone. Volume specifics:
`docs/runbooks/orphan-volume-cleanup.md`. NOT for routine teardown — CI uses
the cluster-scoped `llz ci reap-volumes` / `reap-nodebalancers`.

## Hard rules

- **Never `kubectl delete` the `lke-admin-token` secret** — rotation happens
  only via the Linode delete-kubeconfig API (`llz credentials lke-admin
  rotate`; see `docs/lessons-learned.md` and
  `docs/runbooks/lke-admin-rotation.md`).
- `reap` deletes real cloud resources: always show the dry-run output and get
  explicit confirmation before `CONFIRM=yes` / `--yes`.
- Tags are immutable — a fix means a NEW pre-release tag, never a moved one.
