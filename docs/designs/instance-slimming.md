# Design: slim the instance surface + smooth upgrades

**Status:** Lever 1 landed (this PR). Levers 2–3 are the staged, e2e-gated rollout
this doc specifies.
**Relates to:** `docs/designs/cross-org-reuse-pattern.md` (#201/#202 — why the
reusable bodies are instance-local), `tools/cmd/llz/commands.go` (`runUpgrade`).

## Problem

Every instance carries **~4,875 lines of workflow YAML** (149 files, ~13.6k lines
total). The bulk is four large reusable bodies copied into each instance:

| file | lines |
|---|---|
| `llz-terraform.yml` | 1,613 |
| `llz-bootstrap-openbao.yml` | 1,082 |
| `llz-scheduled-checks.yml` | 665 |
| `llz-secret-rotation.yml` | 630 |

These are `managed` (re-rendered wholesale on `copier update`, so they never
merge-conflict), but a template bump still churns all of them — a large,
scattered diff the operator hand-commits, and where a botched 3-way merge once
shipped conflict markers (the gsap-apl incident).

**Root cause:** #201 *designed* "local job graph + central step logic" so
instances stay thin. #202 delivered cross-org correctness the fast way — copying
the entire reusable *bodies* into every instance. Correct, but not thin. The
copied-in bulk is the felt cost.

`llz-terraform.yml` composition (the exemplar): 19 jobs, 85 steps — **45 already
`llz ci` calls**, 21 inline-bash blocks, 16 composite-action `uses:`. So it's a
job graph with the step logic half-assimilated.

## The cross-org floor

Cross-org secrets require the **job graph** (`environment:` + `needs:` + matrix)
to live in the instance caller — a reusable workflow `uses:` can't enter an
environment cross-org, and a composite action can't declare jobs/`environment:`.
So the irreducible instance surface is: the job graph + its `environment:`/secret
wiring. Everything *inside* a step can move out (into the `llz` container or a
central composite action). Target end-state: **thin job-graph callers, one call
per job, zero copied-in step bodies.**

## Lever 1 — smooth the upgrade (LANDED)

`llz upgrade` now: (a) gates on leftover conflict markers and fails loudly before
commit; (b) prints a `vX → vY` summary + diffstat; (c) `--commit` records the
whole upgrade as one labeled `chore(template): upgrade vX → vY` commit. This
doesn't shrink the surface but kills the felt pain — the operator reviews one
diff, never edits these files, and can't silently ship markers.

## Lever 2 — assimilate step logic into `llz ci` verbs (STAGED)

Move the 21 inline-bash blocks (and repeated step sequences) into tested Go verbs
in the image — versioned, unit-tested, **not copied in**. Each job shrinks toward
`environment:` + `container:` + one or two `run: llz ci <phase>` steps.

**First slices (by reuse count, safest first):**

1. **`llz ci tf-output`** — 8 inline `terraform output` reads across the `llz-*`
   workflows have no verb (unlike `tf-plan`/`tf-apply`/`tf-import`, which exist).
   Read-only ⇒ lowest risk. Add the verb + swap the 8 sites.
2. **`llz ci tf-destroy`** — 5 inline `terraform destroy` blocks; completes the
   `tf-*` family. Destroy-path ⇒ swap under the existing `assert-destroy-confirm`
   guard, e2e on the teardown job.
3. **Collapse plan/apply/wait sequences** — e.g. an `apply-cluster <region>`
   phase verb that folds render-tfvars → tf-apply → wait-cluster-ready →
   endpoint-echo (today separate steps) into one call.

**Guardrail:** each slice is behavior-preserving (the verb wraps the same
`terraform`/`kubectl` invocation) and lands in **its own e2e-gated PR** — a green
`release-e2e` (the real cluster) is the acceptance test, because a workflow step
change can't be proven by unit tests alone. Projected: `llz-terraform.yml` 1,613
→ ~500 lines once the inline blocks and step sequences are folded.

## Lever 3 — per-job bodies → central composite actions (STAGED)

Replace each job's copied-in step body with `uses: <upstream_org>/…/actions/
<phase>` — public, secret-free, secrets passed via `with:`. The heavy bodies then
live **once in the template repo**, not in every instance. The instance keeps only
the job graph (it must — see the floor) + one `uses:` per job.

**Constraint:** a composite action is a step sequence within ONE job — it can't
express `jobs`/`needs`/`matrix`/`environment`. So the graph structure stays in the
caller; only the per-job step content moves. Landing point: "thin job-graph
callers," not a single caller.

**First slice:** extract one leaf job's body (e.g. the object-storage
plan/apply) into a composite action, wire the caller to it, e2e. Establishes the
pattern (input plumbing, secret passing, versioning at `@<@ llz_version @>`)
before the multi-job rollout.

**Interaction with Lever 2:** do Lever 2 first where possible — a job that is
already `run: llz ci <phase>` needs only a trivial composite-action wrapper (or
none), so assimilating into the CLI often *removes the need* for a composite
action at all. Lever 3 is for the residual YAML orchestration the CLI can't own.

## Rollout order

1. **Lever 1** (this PR) — immediate relief, zero pipeline risk.
2. **Lever 2 slices** — one verb/phase per PR, each `release-e2e`-gated. Start
   read-only (`tf-output`), then destroy, then the plan/apply/wait collapses.
3. **Lever 3 slices** — only for orchestration the CLI can't absorb; one job per
   PR, `release-e2e`-gated.

Each slice is independently shippable and measurable (lines removed from the
copied-in surface), so the instance thins incrementally with a green e2e at every
step — no big-bang pipeline rewrite.
