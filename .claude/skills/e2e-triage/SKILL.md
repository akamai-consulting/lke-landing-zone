---
name: e2e-triage
description: Diagnosing a red release-e2e run or a wedged cluster. Use when an `llz ci assert-*` lane fails, when converge or the health tree hangs, when a bootstrap never finishes, or when a cluster create/destroy stalls. Routes to the classifiers that already name the failure class before spending a round on a live cluster.
---

# Triaging a red e2e lane

A live-cluster round is the most expensive debugging step in this repo: a
dispatch, a cluster, a bill, and a wait during which the step logs are not
readable. **Everything below is ordered to avoid needing one.**

[`docs/runbooks/e2e-lane-diagnostics.md`](../../../docs/runbooks/e2e-lane-diagnostics.md)
is canonical for the access mechanics. This file is the order of operations.

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
`tools/internal/health/argo.go` and `tools/cmd/llz/ci_health.go`, with the
reasoning in comments beside each verdict. Do not re-derive them by hand, and do
not add a rival table to this file that would rot — read the code:

| Verdict | Means |
|---|---|
| `CatPending` + argocd-redis cache auth | repo-server↔redis password split — **transient**, polling |
| `CatPending` + 256KB annotation limit | converge strips the oversized CRD annotation and re-polls |
| `CatPending` + waiting on OpenBao bootstrap | ordering, not a fault |
| `CatDeferred` | deliberately not converged yet |
| `CatInstance` | instance-owned via the operator escape hatch — reported, does **not** gate |
| `CatDrift` | workload functional; drift only |
| `CatFail` | a real spec error |

The distinctions are load-bearing. `IsRepoServerCacheAuthError` exists because a
`WRONGPASS`/`NOAUTH` `ComparisonError` makes **every** Application fail at once
and looks like a fleet-wide config disaster; it is one never-restarted redis pod
holding a stale `--requirepass` after the password rotated under it. The realign
is `rollout restart` on the **redis** side. `IsGitAuthError` is separate because
one downstream run burned its entire 1200s convergence budget polling a git
credential refusal as if it were a network flake.

**If the health tree is frozen with every app Degraded or Unknown, suspect a
shared cache/auth split before suspecting the apps.**

## Step 3 — the wedges with their own runbook

Do not rediscover these. Match the symptom, then follow the file:

| Symptom | Runbook |
|---|---|
| apl-operator Running 1/1, 0 restarts, helmfile never progresses; sealed-secrets `ErrUnsealFailed` | [`apl-branch-recreate-wedge.md`](../../../docs/runbooks/apl-branch-recreate-wedge.md) |
| apl-values changes not reaching the cluster | [`apl-values-propagation.md`](../../../docs/runbooks/apl-values-propagation.md) |
| Volumes leaked / reaper reports "none matched" | [`orphan-volume-cleanup.md`](../../../docs/runbooks/orphan-volume-cleanup.md) — **always pass `--env`**; without it the renamed volumes leak |
| reconciler gauges stale or the elector never acquires | [`reconciler-alerts.md`](../../../docs/runbooks/reconciler-alerts.md) |

`Running 1/1` with healthy endpoints is the **normal** appearance of most wedges
here. It is not evidence of anything.

## Step 4 — the failure modes that are not in a runbook

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

## Step 5 — only now, a live cluster

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

## Step 6 — read the producer's config, not the consumer's error

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

## Before you close it out

- Tear the cluster down. Manual teardown needs the confirm token and only targets
  the cluster module; databases and object storage persist between runs by design.
- Delete any `debug/*` branch you pushed to the instance repo. Its default branch
  is overwritten by every e2e instantiate; stray branches are not.
- If the root cause was a class not yet encoded, put it where the next reader will
  hit it: a classifier verdict, a lane's `Why`, or the scars list — **not** a new
  table in this file.
