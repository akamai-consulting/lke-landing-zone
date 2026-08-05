---
name: e2e-triage
description: Triage a failed release-e2e run or a wedged cluster - map the failure to a known wedge class, read the lane's verdict, and clean up leaked Linode resources (orphan clusters, NodeBalancers, VPCs, volumes). Use when an `llz ci assert-*` lane fails, when converge or the health tree hangs, when a bootstrap never finishes, when a fresh cluster-create hangs, or when an e2e cycle leaks resources.
---

# Triaging a red e2e lane

`release-e2e.yml` stands up a REAL LKE-Enterprise cluster (instantiate → provision
→ validate → destroy) — slow and billable, so triage before rerunning. A
live-cluster round is the most expensive debugging step in this repo: a dispatch,
a cluster, a bill, and a wait during which the step logs are not readable.
**Everything below is ordered to avoid needing one.**

[`docs/runbooks/e2e-lane-diagnostics.md`](../../../docs/runbooks/e2e-lane-diagnostics.md)
is canonical for the access mechanics. This file is the order of operations.

## Step 0 — get the failure

```bash
gh run list --workflow=release-e2e.yml --limit 5
gh run view <run-id> --log-failed
```

## Step 1 — read the lane's own verdict, not the symptom

Every lane carries a `Why` string saying what it proves **and what stays green
without it**. It prints with the lane's group. Read that first: it usually
converts "no records in the window" into a named pipeline with an ordered list of
things to check.

```bash
cd tools && go run ./cmd/llz ci assert-suite --list
```

If several lanes went red at once, that is a signal in itself — a shared
dependency (cluster access, DNS, the whole health tree) failed, not each lane.

## Step 2 — let the classifiers name it before you guess

Large classes of wedge are **already encoded** in
`tools/internal/health/argo.go` and `tools/cmd/llz/ci_health.go`. Each verdict
carries its reasoning in a comment beside it, and those comments are the
authority — this is an index into them, not a substitute, and when the two
disagree the code is right:

| Verdict | Roughly |
|---|---|
| `CatPending` | transient — poll. Redis cache auth, the 256KB annotation limit, waiting on OpenBao bootstrap, a Progressing rollout |
| `CatDeferred` | deliberately not converged yet |
| `CatInstance` | instance-owned via the operator escape hatch — reported, does **not** gate platform convergence |
| `CatDrift` | workload functional; drift only |
| `CatFail` | a real spec error |

**Read the comment attached to whichever verdict you got** — the distinctions are
load-bearing. `IsRepoServerCacheAuthError` exists because a
`WRONGPASS`/`NOAUTH` `ComparisonError` makes **every** Application fail at once
and looks like a fleet-wide config disaster; it is one never-restarted redis pod
holding a stale `--requirepass` after the password rotated under it. The realign
is `rollout restart` on the **redis** side. `IsGitAuthError` is separate because
one downstream run burned its entire 1200s convergence budget polling a git
credential refusal as if it were a network flake.

**If the health tree is frozen with every app Degraded or Unknown, suspect a
shared cache/auth split before suspecting the apps.**

## Step 3 — is it a class that already has a guard?

If one of these recurs, **the guard has a gap — fix the guard, not just the
symptom** (see the `add-ci-guard` skill). The Makefile comment above each target
documents the original wedge:

| Symptom | Class | Guard |
|---|---|---|
| platform-bootstrap sync stuck before OpenBao (wave 0) | negative-wave kind not health-inert (PR #142) | `wave-health-guard` |
| workload never Healthy, later-wave ExternalSecrets starved | workload waves before the ExternalSecret it needs (#163) | `wave-dependency-guard` |
| cross-namespace traffic to harbor silently dropped | egress into an Istio STRICT-mesh namespace | `mesh-egress-guard` |
| metrics unscraped / alerts never fire, everything else green | monitor/rule CR missing `prometheus: system` (#175) | `monitoring-label-guard` |
| Argo 404s a chart version on cold bootstrap; support-plane stranded | pin never published | `chart-pin-guard` / `chart-version-guard` |
| an ExternalSecret on an apiVersion apl-core stopped serving | `platform-apl/` is invisible to the rendered-chart gates | `dropped-apiversions-check` |

`docs/architecture/convergence-contract.md` defines what "converged" is supposed
to mean, which is worth re-reading before declaring something wedged.

## Step 4 — the wedges with their own runbook

Do not rediscover these. Match the symptom, then follow the file:

| Symptom | Runbook |
|---|---|
| apl-operator Running 1/1, 0 restarts, helmfile never progresses; sealed-secrets `ErrUnsealFailed` | [`apl-branch-recreate-wedge.md`](../../../docs/runbooks/apl-branch-recreate-wedge.md) |
| apl-values changes not reaching the cluster | [`apl-values-propagation.md`](../../../docs/runbooks/apl-values-propagation.md) |
| Volumes leaked / reaper reports "none matched" | [`orphan-volume-cleanup.md`](../../../docs/runbooks/orphan-volume-cleanup.md) — **always pass `--env`**; without it the renamed volumes leak |
| reconciler gauges stale or the elector never acquires | [`reconciler-alerts.md`](../../../docs/runbooks/reconciler-alerts.md) |

`Running 1/1` with healthy endpoints is the **normal** appearance of most wedges
here. It is not evidence of anything.

## Step 5 — the failure modes that are not in a runbook

From [`docs/lessons-learned.md`](../../../docs/lessons-learned.md) — read the
"Operational scars" section in full before a live round. The ones that most often
present as an unrelated symptom:

- **Silent cluster-create hang.** `Still creating…` to the job timeout means the
  node pool never provisioned. It is **not** reliably orphans — it has been hit
  with `Orphaned total: 0`. Confirmed root cause once: VPC-quota exhaustion from a
  per-cycle leak. Ask the API which resources exist rather than sweeping on a guess.
- **A NetworkPolicy matching nothing** looks exactly like a healthy pod. Post-DNAT
  evaluation, aggregated-APIService webhooks on `:443`, and upstream label renames
  are all in the scars list, and all present as a hang somewhere else.
- **Converge polling forever on an APIService** is usually a dropped discovery
  probe, not a slow component.

## Step 6 — only now, a live cluster

> **Ask the operator before dispatching.** Everything below stands up real,
> billable Linode infrastructure on the instance repo's account, and a kept
> cluster bills until someone removes it. Steps 0–5 are free; this one is not.
> Confirm it is wanted, and say what it will cost in time and resources.

Two things do **not** work, and both cost a round:

- **Your own Linode token is the wrong account.** The e2e clusters belong to the
  instance repo's `infra-e2e` GitHub Environment. A personal token authenticates
  fine and then 404s. That 404 is an authorization answer wearing a not-found
  costume — do not read it as "the cluster is gone".
- **The cluster is already destroyed.** The lane tears down on every path,
  including failure. You must have asked for it to be kept *in advance*:

```bash
gh workflow run release-e2e.yml --ref <branch> -f dry_run=false -f keep_cluster=true
```

The teardown step then shows `skipped` while the job still reports success —
**read the step, not the job**. It bills until you remove it.

For getting `kubectl` at it, follow the branch-injection recipe in
[`e2e-lane-diagnostics.md`](../../../docs/runbooks/e2e-lane-diagnostics.md). Two
things that waste a round once you are in: port-forwards race a cold ACL (poll
until the target answers, and make sure the endpoint you poll exists), and
`gh run view --log` returns almost nothing mid-run even for completed steps.

## Step 7 — read the producer's config, not the consumer's error

The failures that cost the most rounds were all one shape: **the gate named what
it could not find, and the answer was what the other side was actually doing.**

- Loki's tenant came from apl-core's collector config in the `otel` namespace,
  not from anything Loki said.
- The object-storage Secret names came from listing Secrets, not from the ref
  that was missing.
- Harbor's mTLS mode is PERMISSIVE **on purpose** (ADR 0010 step 3), so a
  plaintext dial succeeding there is correct behavior, not a finding.

If a lane says "X is absent", the next command is almost always "what is
present". Where a gate does not print that itself, **that is a gap worth closing
rather than a cluster worth standing up** — see the `gate` skill.

## Clean up leaked resources before rerunning

Failed and cancelled cycles leak Linode resources, and the backlog is what makes
the **next** cluster-create hang. Sweep before you rerun:

```bash
# DRY-RUN by default; needs LINODE_TOKEN; REGION recommended
make reap-orphans REGION=<region> CLUSTER_LABEL=<label>
```

Sweeps in dependency order — clusters (if `CLUSTER_LABEL`) → firewall →
NodeBalancers → VPCs → Volumes. Volume specifics:
[`orphan-volume-cleanup.md`](../../../docs/runbooks/orphan-volume-cleanup.md).
NOT for routine teardown — CI uses the cluster-scoped `llz ci reap-volumes` /
`llz ci reap-nodebalancers` instead.

> **`reap` deletes real cloud resources.** Show the operator the dry-run output
> and get an explicit go-ahead before re-running with `CONFIRM=yes`. Never sweep
> on a hunch: a hang is not proof of orphans, and it has been diagnosed as
> VPC-quota exhaustion on an account reporting `Orphaned total: 0`.

## Hard rules

- **Never `kubectl delete` the `lke-admin-token` Secret.** On LKE-Enterprise it is
  not regenerated. Rotation happens only via the Linode delete-kubeconfig API,
  which is what `llz credentials lke-admin rotate` drives — see the
  `rotate-credentials` skill and
  [`lke-admin-rotation.md`](../../../docs/runbooks/lke-admin-rotation.md).
- **Tags are immutable.** A fix means a NEW pre-release tag, never a moved one.

## Before you close it out

- Tear the cluster down. Manual teardown needs the confirm token and only targets
  the cluster module; databases and object storage persist between runs by design.
- Delete any `debug/*` branch you pushed to the instance repo. Its default branch
  is overwritten by every e2e instantiate; stray branches are not.
- If the root cause was a class not yet encoded, put it where the next reader will
  hit it: a classifier verdict, a lane's `Why`, a guard (the `add-ci-guard`
  skill), or the scars list — **not** a new table in this file.
