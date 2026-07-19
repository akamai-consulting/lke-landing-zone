# Design: slim the instance surface + smooth upgrades

**Status:** Levers 1 and 1.5 landed. Levers 2–3 are the staged, e2e-gated rollout
this doc specifies — but see **Re-ranking (measured)** below: the largest single
win is not in Lever 2.
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

A template bump churns all of them — a large, scattered diff the operator
hand-commits, and where a botched 3-way merge once shipped conflict markers (the
gsap-apl incident).

> **Correction (was wrong in the original draft).** This section used to claim the
> bodies "are `managed` (re-rendered wholesale on `copier update`, so they never
> merge-conflict)". They were not: `.template-manifest` classified all of
> `.github/workflows/**` as `merge`, and copier has no overwrite mode — it
> re-renders and 3-way-merges everything. So a hand-edited body *could* conflict,
> and nothing detected drift. Lever 1.5 makes the original claim true.

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

## Lever 1.5 — make the manifest classes real (LANDED)

Lever 1 made the upgrade *reviewable*; this makes it *authoritative*.

- **`.template-manifest` split.** `.github/workflows/**` stays `merge` for the
  caller stubs (they carry 2–9 `<@ token @>` pins each), but the vendored
  `llz-*.yml` bodies are now `managed` — every one is token-free, so an instance's
  copy is byte-identical to the render unless someone edited it.
- **`llz upgrade` enforces the classes.** Copier has one strategy (re-render +
  3-way merge) for the manifest's three, so `runUpgrade` now snapshots `owned`
  files before copier and restores them after, then overwrites `managed` files
  from a clean render of the target ref (`applyUpgradeManifestPolicy`). Instances
  with no usable manifest upgrade exactly as before.
- **`llz ci workflows-fresh` drift guard** — the check
  `cross-org-reuse-pattern.md` specified and nobody built. It runs inside
  `llz lint` (so it reaches instances without editing the vendored workflows it
  protects) and compares the `.github/` surface against digests the template ships
  in `.template-workflows.lock`.

  It is a **hash lock, not a re-render**: `copier` exists only in the devcontainer
  image, while the reusable workflows run in `ci-terraform` — and an air-gapped GHE
  has no route to the template repo anyway (ADR 0003). Digests work offline. This
  is sound only because the covered files are token-free, which is the same fact
  that lets them be `managed`; `--write` refuses to lock a token-bearing file, so
  the two decisions cannot silently drift apart.

**Why this gates the rest:** a large body change (Lever 2, or moving maintainer
rationale out of the YAML) previously meant 3-way-merging thousands of lines in
every instance. Now it lands as a clean overwrite, and any instance that edited a
body is *told* rather than silently clobbered.

## Re-ranking (measured)

A line-level census of the vendored corpus (4,116 lines across the 7 `llz-*.yml`)
changes the priority order the original draft assumed:

| category | lines | share |
|---|---|---|
| comments | 1,574 | **38.2%** |
| YAML structure (jobs/steps/`with`/`if`/`needs`) | 1,824 | 44.3% |
| **inline shell (`run:` bodies)** | **374** | **9.1%** |
| shell-block comments | 109 | 2.6% |
| blank | 235 | 5.7% |

56 of 73 `run:` steps are already single-line `llz` calls. **The shell push-down
is largely done** — Lever 2's entire remaining ceiling is ~426 lines. The single
biggest lever is instead the **1,574 lines of maintainer rationale** copied
verbatim into every instance, which outweighs every push-down combined by ~4×.

Do **not** implement that as a render-time comment strip — `copier.yml` rejects
that for a real reason (a `_task` runs after the merge-base render, so every
comment line would diverge). Author the bodies lean *in template source* and move
the rationale to a template-repo sidecar.

## Lever 2 — assimilate step logic into `llz ci` verbs (STAGED)

Move the remaining inline-bash blocks into tested Go verbs in the image —
versioned, unit-tested, **not copied in**.

**Corrected slice list.** The original three slices were measured against a
snapshot that has since moved:

1. ~~`llz ci tf-destroy`~~ — **already done, delete this slice.** The "5 inline
   `terraform destroy` blocks" are now all *comments*; destroy runs through
   `assert-destroy-confirm` / `tf-plan --destroy` / `destroy-unwedge` /
   `reap-nodebalancers`.
2. **`llz ci tf-output`** — real, but **6 sites, not 8**. Read-only ⇒ lowest risk.

**The actual top targets, by lines eliminated** (none were in the original list):

| target | lines | verb |
|---|---|---|
| `llz-terraform.yml` S3 bucket drain | 102 | `llz ci drain-buckets` — also deletes a pinned `s5cmd` tarball download + sha256 preamble; a Go impl speaks S3 natively |
| `linode-credentials/action.yml` (4 near-duplicate blocks) | ~109 | `--gh-output` / `--gh-summary` on `llz credentials` |
| `llz-bootstrap-openbao.yml` CronJob admission preflight | 47 | `llz ci preflight-cronjob-admission` |
| `llz-cluster-health.yml` health block | 47 | `llz ci health --gate --summary-out` |
| `llz-bootstrap-openbao.yml` e2e assert fan-out | 37 | `llz ci assert-suite` (today a bash job-runner over 8 existing verbs) |
| `llz-scheduled-checks.yml` PrometheusRule check | 28 | `llz ci assert-prometheusrules` |
| `llz-wedge-gameday.yml` | 24 | `--summary-out` flag |
| `llz-terraform.yml` bucket summary | 17 | `llz ci summarize-buckets` |
| `llz-cluster-health.yml` argo probes | 15 | fold into existing `llz ci diagnose-argocd` |

`linode-credentials/action.yml` is the priority regardless of line count: it is
the one place YAML still owns security-relevant logic (token masking, the sha256
cross-job handoff, the dry-run branch).

**Verbatim duplication to remove alongside:** the 180 s apiserver wait loop is
copied byte-for-byte into `llz-cluster-health.yml` and `llz-wedge-gameday.yml`
(and `llz ci converge` already absorbs it internally); the `infra-<region>` secret
preflight is duplicated verbatim across `llz-terraform.yml` and
`llz-bootstrap-openbao.yml`; the `llz render --tfvars-only` guard appears at 3
call sites.

**Job-graph collapses** (structure, not shell): `tf-lint` + `checkov` +
`promote-pipeline-drift` share an `if:` and have no `needs:`/`environment:` → one
job; `push-noop-notice` is a 27-line job that echoes that push does nothing →
delete; three `llz-scheduled-checks.yml` matrix jobs share an `if:` and preamble →
one job, which also removes **2 redundant control-plane ACL open/close cycles per
region per run** (the real argument, not the ~75 lines).

**Guardrail:** each slice is behavior-preserving and lands in **its own e2e-gated
PR** — a green `release-e2e` (the real cluster) is the acceptance test, because a
workflow step change can't be proven by unit tests alone.

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

**Open question that gates this lever.** Lever 3 assumes the instance must not
fetch from the template repo at runtime — the reachability half of ADR 0003. That
half is *assumed, not validated*: `forge-abstraction.md` §Non-goals records
"Settled: the cluster can reach github.com", and instances already depend on the
public GHCR image `vars.TF_IMAGE` at runtime, so "depends on nothing outside
itself" is not literally true today. The *cross-org secrets* half of ADR 0003 is
real and was reproduced live — that is not in question and must not be
relitigated. Resolve the reachability question **before** starting Lever 3: if
github.com reachability is acceptable, Lever 3 gets much cheaper and subsumes a
large part of Lever 2.

## Rollout order

1. **Lever 1** — immediate relief, zero pipeline risk. *(landed)*
2. **Lever 1.5** — manifest classes enforced + drift guard. *(landed)*
3. **Rationale extraction** — the 1,574-line comment mass, in template source (not
   a render-time strip). Largest single win; no runtime behavior change, so it
   needs review rather than a full e2e.
4. **Lever 2 slices** — one verb/phase per PR, each `release-e2e`-gated. Batch by
   blast radius: read-only summaries first, then the verbatim-duplication removals
   and job-graph collapses, then the two that carry real risk (`drain-buckets` on
   the destroy path, and `linode-credentials`).
5. **Lever 3 slices** — only after the reachability question above is settled; one
   job per PR, `release-e2e`-gated.

Each slice is independently shippable and measurable (lines removed from the
copied-in surface), so the instance thins incrementally with a green e2e at every
step — no big-bang pipeline rewrite.
