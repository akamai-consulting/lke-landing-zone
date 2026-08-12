# `llz-scheduled-apply` — maintainer rationale

`instance-template/.github/workflows/llz-scheduled-apply.yml` is the reusable
body behind the `scheduled-apply.yml` caller stub. It applies every deployment's
Terraform on a cadence and blocks on the convergence gate that follows.

Like every `llz-*.yml`, it is vendored verbatim into each instance where it can
never be updated in place, so the archaeology lives here rather than in the YAML.

---

## The gap it closes

A merged change reaches an instance's cluster by three routes, and only two of
them are pull-based:

| What moved | How it lands | Latency |
|---|---|---|
| `apl-values/_shared/apl-overlay/**` | in-cluster `llz-reconciler`, `--reconcile-apl-overlay` git-syncs it onto `apl-<env>` | continuous |
| `apl-values/<env>/manifest/**` | Argo, from the repo | continuous |
| `terraform-iac-bootstrap/**`, the module `?ref=` | **nothing** | until a human runs `llz build` |

The third row is the whole reason this workflow exists. A push to `main`
deliberately neither plans nor applies — `llz-terraform.yml`'s
`push-noop-notice` job exists purely to say so out loud — and the apply is a
`workflow_dispatch` an operator fires by hand. So a `llz upgrade` that moves the
Terraform module ref merges, passes every check, and then sits undeployed with
nothing anywhere reporting that fact.

## Why it dispatches rather than calling the pipeline

The obvious implementation is `uses: ./.github/workflows/llz-terraform.yml` with
`action: apply`. It does not work, and it fails in the worst available way.

Every apply job in that pipeline is gated on
`github.event_name == 'workflow_dispatch'`. Under a scheduled caller the event is
`schedule`, so each of those `if:` expressions is false — the reusable is invoked,
its job graph is evaluated, every apply job **skips**, and the run goes green
having applied nothing. A workflow whose failure mode is "reports success for
work it did not do" is worse than no workflow.

The alternative was to relax those guards to admit `schedule`. That was
considered and rejected: it widens the set of events that can reach a production
apply, in the most dangerous workflow an instance carries, permanently, so that a
convenience feature can reuse it. Dispatching keeps the guard exactly as strict as
it is today. This workflow reaches the apply the same way an operator does —
through `llz build` — and the dispatched run still carries the
`environment: infra-<deployment>` approval on `apply-cluster`.

**The cost of that choice is a PAT.** `GITHUB_TOKEN` cannot trigger a workflow:
GitHub suppresses runs from events raised with it, as a recursion guard. So the
dispatch needs `LLZ_AUTOMATION_TOKEN` (see [../secrets.md](../secrets.md)). That
is a real cost — a credential with `actions: write` on the repo — and it is the
honest price of not loosening the apply guard.

## Why `llz build --watch` and not `gh workflow run`

`gh workflow run` is fire-and-forget: it prints "Created workflow_dispatch event"
and exits 0. A scheduled job built on it would go green the moment the dispatch
was *accepted*, which says nothing about whether the apply succeeded.

`--watch` (added with this workflow) blocks on the resulting run and exits
non-zero unless it concluded `success`. Identifying *which* run is the whole
problem — GitHub registers it asynchronously, so "newest dispatch run" is a
completed run from days ago for the first few seconds. `internal/shared/dispatchwatch` records
the newest run id **before** dispatching and accepts only a strictly higher one;
`--watch` reuses that machinery rather than re-deriving it.

Every "could not tell" is an error, not a pass:

| Situation | Verdict |
|---|---|
| run concluded `success` | pass |
| run concluded anything else | fail, **naming the conclusion** — `startup_failure` and `failure` have nothing in common as remedies |
| GitHub never registered the run | fail, explicitly as a *lost handle*, not a failed apply — so nobody re-dispatches on top of a live apply |
| status unreadable for one poll | retry, bounded by the attempt budget |
| still running when the budget expires | fail |
| parked on a deployment approval | fail after a ~5-minute grace, **naming the approval** — see below |
| watcher never armed | fail |

The budget is counted in **attempts, not wall-clock**, because `sleepFn` is a test
seam: a `time.Now()` deadline plus a no-op sleep is a busy-spin that never
terminates. Same lesson as `bootstrapDeps`.

**The approval arm has its own, much shorter budget**, and that is the one that
matters here. `apply-cluster` sits behind `environment: infra-<deployment>`, and
this repo's own docs tell adopters to put required reviewers on
infra-staging / infra-prod — so an unattended 04:00 dispatch parks on `waiting` by
design. On the shared 3h budget that would report "did not finish within the watch
budget", which names the wrong problem and holds a runner for three hours to reach
it. A ~5-minute grace still covers a human who is watching and approves promptly;
past that the failure says the run needs an approval nobody is going to give.

So an armed scheduled apply and required reviewers on the same environment are in
tension **by design** — the workflow now says so out loud rather than timing out.
Either leave the reviewers and treat the weekly run as a drift *report* that fails
loudly, or drop them for deployments meant to apply on a timer.

## Why it ships disarmed

`LLZ_SCHEDULED_APPLY` defaults unset, and an unarmed run applies nothing and says
how to arm it.

Applying production infrastructure on a schedule is a decision an adopter makes.
A delivered workflow that started doing it the moment they ran `llz upgrade`
would be a template release changing an instance's runtime behaviour unasked —
precisely what the `managed`/`owned` split exists to prevent. The workflow is
`managed` (we own its body); the choice to run it is a repo variable, which we do
not.

## Running it repeatedly is the feature

`tofu apply` is idempotent and the dispatched chain ends in `llz ci converge`. So
a week in which nothing changed still proves the cluster is reachable, converged,
and matches the committed spec — which catches console edits and drifted firewall
rules that no merge-triggered run would ever look at.

**It also passes `--assert-invariants`, and that is the half worth arguing for.**
Converge proves the health tree settled. It says nothing about whether the
volumes are still encrypted, whether Loki is still S3-backed, or whether Managed
Postgres still accepts the credential it was seeded with — invariants that rot
*between* applies, silently, which is precisely the window a weekly no-op run is
the only thing watching. Without it a green week means "nothing crashed".

That input was called `assert_loki` until this change, and named one of the four
things it gates. A promotion into prod skipped the volume-encryption check with
nothing on screen saying so.

That is also why the cron sits two hours after `scheduled-checks.yml`'s weekly
health probe: reading back through a red Sunday, the health verdict that preceded
the apply is already in the log.

## The `verdict` job is not decoration

`needs.apply.result` collapses the matrix to one value, and the verdict job turns
it into a single check worth marking required.

Without it, a matrix with **zero legs** — no deployments discovered, or a
narrowing dispatch input that matched nothing — reports success for having done
nothing at all, which on a dashboard is indistinguishable from a clean week. An
armed instance that discovers no deployments is a misconfiguration, and the
verdict job fails on it rather than shrugging.

## What is unproven

The release-e2e lane does **not** drive this workflow; it dispatches
`terraform.yml` directly, which is the same apply one hop earlier. What that
leaves untested is the stub's own trigger surface and the armed/verdict gating.
`reusable-workflow-caller-permissions` covers the `startup_failure` class
statically — the failure that left `secret-rotation.yml` dead for months — and
the `--watch` fail-closed arms are unit-tested in `internal/shared/dispatchwatch/wait_test.go`.

See `tools/internal/cli/delivered_workflow_coverage_test.go`'s
`exercisedEntryPoints` for the written-down version of that decision.
