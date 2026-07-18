# E2E timing instrumentation — make "where did the time go?" a query

## Problem

Every e2e speed-up decision so far has been gated on the same missing thing:
we do not know where the time goes, so each "how much would X save?" turns into
manual log archaeology. Concretely, we could not answer without hand-reading one
run's scattered log timestamps:

- Is the ~6m apl-core foundation install pull-bound or render-bound? (determines
  whether a pre-pull is worth building at all)
- How much of the ~16m "Apply Cluster" is Linode-side provisioning vs terraform
  overhead? (sets the ceiling on the HA-control-plane-off experiment)
- What is actually the converge tail's long pole, and did it move after the
  harbor-kick / store-recovery changes?

Today the only timing signal is GitHub's coarse job/step durations plus whatever
one reconstructs from log lines. There is **no standing, machine-readable,
success-path timing** — and the existing diagnostics (`diagnose-argocd`, the
loki-gateway capture) are all *failure*-triggered, so a green run records nothing
about where its minutes went.

## Principle

Instrumentation must be **always-on, machine-readable, and diffable**:

- **Always-on** — collected on every run (including the cheap warm-fast-path runs),
  because e2e is slow to iterate; you want data from the runs already happening,
  and you cannot optimize what you only capture on failure.
- **Machine-readable + diffable** — a JSON artifact per run, so an A/B (HA-on vs
  HA-off, before/after a change) is a diff, not a re-read. This IS the measurement
  infrastructure the deferred experiments need.
- **Near-zero cost** — a boundary mark is an append; collectors are read-only
  `kubectl get` + parse; every piece is best-effort (a failure is a note, never a
  gate).

## What it collects

### Tier 1 — the phase timeline (foundation)

- `llz ci phase-mark <label>` appends `{label, ts_ms}` to a shared per-job log
  (`$LLZ_PHASE_LOG`, pointed at `$RUNNER_TEMP` so it survives across a job's
  steps). The workflow drops one at each phase boundary.
- `llz ci phase-report --out timeline.json` reads the marks, computes each
  consecutive interval's duration, writes a table to `$GITHUB_STEP_SUMMARY`, and a
  JSON timeline the job uploads as an artifact. N marks → N-1 intervals, each
  labeled by the mark that opens it.
- `llz ci collect-image-pulls --out pulls.json` parses the cluster's kubelet
  `Pulled` events into per-image pull durations (+ total) — **the direct answer to
  pull-bound-vs-render-bound**. (Pulls across nodes run in parallel, so the sum
  overcounts wall-clock; the signal is per-image magnitude vs the phase length.)

The wait verbs' internal timing is delivered by the marks that bracket them (a
mark before/after each phase), rather than editing each verb — one uniform model.

### Tier 2 — targeted at the two big blocks

- **apl-core install breakdown**: a raw dump of the `apl-operator` pod logs into
  the artifact bundle (its helmfile phase markers — clone/render/apply). The
  operator's exact log format is not assumed; we collect the raw logs first so a
  parser can be written against real output once, rather than guessing now.
- **Create timing**: `phase-mark` brackets the `tf-apply` cluster step, so the
  timeline carries the precise LKE-E create wall-time (cleaner than the job-level
  number, which folds in container-init + init + plan) — the baseline the HA-off
  A/B compares against.

### Tier 3 — converge long-pole

`llz ci converge` now records the Pending+Failed set on each in-progress poll and,
on convergence, reports what was still not-OK on the last poll before it converged
— the tail that gated the run — to the step summary + log. Confirms the tail's
identity across runs instead of assuming it is harbor.

## Layout

The workflow points `LLZ_PHASE_LOG=$RUNNER_TEMP/llz-phases.jsonl` at the job level,
drops `phase-mark`s at boundaries, runs the collectors after converge, calls
`phase-report --out`, and uploads a `<job>-timing` artifact (timeline.json +
pulls.json + apl-operator.log). All steps are `if: always()` / best-effort so
instrumentation never changes a run's outcome.

## Non-goals / honesty

- The sum of image-pull durations is **not** wall-clock (cross-node parallelism);
  read magnitudes, not the sum.
- The apl-operator breakdown is a raw-log dump for now, not a parsed timeline —
  deliberately, until we have seen the real format.
- This measures; it does not optimize. Its whole point is to turn the next
  "measure first" into a lookup, so the HA-off and pre-pull questions get decided
  on data.
