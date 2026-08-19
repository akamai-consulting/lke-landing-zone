# `llz-template-upgrade` — maintainer rationale

`instance-template/.github/workflows/llz-template-upgrade.yml` is the reusable
body behind the `template-upgrade.yml` caller stub. It runs `llz upgrade` in CI
and opens the result as an ordinary pull request.

Like every `llz-*.yml`, it is vendored verbatim into each instance where it can
never be updated in place, so the archaeology lives here rather than in the YAML.

---

## The gap it closes

Upstream template releases reached an instance only when an operator ran
`llz upgrade` on their own machine. Nothing in CI did it.

The instance did have a **monthly template-drift report**
(`llz-scheduled-checks.yml`'s `template-drift` job, `llz drift`), but it is
`permissions: contents: read` by construction — it can only annotate "N commits
behind" and print a compare URL. An instance could therefore sit twelve commits
behind for a year while a green scheduled job faithfully reported it every month.
This workflow is the remedy the report has been describing, which is why its cron
deliberately runs one hour after it, on the same morning.

## Why `llz self-update` runs first

This is the subtle part, and getting it wrong produces a workflow that looks like
it works.

`llz upgrade` with no `--ref` targets the **CLI's own version** — `copier.Ref`
falls through to `ResolveRef`, which reads the binary's anchor `Version`. The
`llz` inside `TF_IMAGE` is the release this instance is already pinned to; that is
what "keep `TF_IMAGE` in step with the template release the instance pins" *means*.

So a bare `llz upgrade` in this job resolves to the current pin and re-renders it
to itself. It exits 0, produces an empty diff, opens no PR, and reads as "no
upstream changes" — every month, forever, while releases pile up. The failure is
silent and looks exactly like success.

`llz self-update` makes the binary the latest published release first, so
`upgrade`'s own default becomes the correct target. That is deliberately the
**documented operator flow** rather than a CI-only ref-resolution path: a
CI-specific way of choosing the target ref is a second implementation that can
drift from the one operators run to reproduce the diff.

**`--ref` does NOT skip the self-update**, and the reasoning that said it should is
the bug it was. An explicit ref needs no resolution — true of the *ref*, false of
the *binary*. Skipping self-update on that path left the upgrade running the llz
baked into `vars.TF_IMAGE`, which lags the pin by design, so `llz upgrade --ref`
would succeed and `llz ci upgrade-pr` would then die on "unknown command".
`--ref` chooses the target; self-update chooses which binary gets there.

## Why full history

`copier update` performs a 3-way merge against the commit recorded in
`.copier-answers.yml`. A shallow clone does not contain that commit, so copier
cannot compute the diff and fails in a way that reads like a template problem
rather than a checkout problem. Hence `fetch-depth: 0`.

## Why a PAT, not `GITHUB_TOKEN`

`GITHUB_TOKEN` can create a pull request. What it cannot do is cause that pull
request to **run anything** — GitHub suppresses workflow runs on events raised
with it.

An upgrade PR that runs no checks is worse than no PR at all, because it *looks*
reviewed. The entire value of routing the upgrade through a PR is that it faces
the same Terraform plan, lint and **repo-readiness** gates a human's upgrade
would, and repo-readiness is specifically what catches a newly mandatory secret
the new release needs and this repo does not have (the `v0.0.42` /
`TF_STATE_ENCRYPTION_PASSPHRASE` case).

So the checkout and the PR both use `LLZ_AUTOMATION_TOKEN`. See
[../secrets.md](../secrets.md) for its scopes. Its lapse is loud in the run and
silent to the operator: an unset token fails the first step with an `::error`,
an expired one fails the checkout — but nobody reads a monthly workflow's runs
between crons, so what is actually observed is upgrade pull requests no longer
arriving, which looks exactly like an instance with no upstream changes. That is
why `token-inventory` measures it daily.

## The pull request opens as a DRAFT, and that is load-bearing

A genuine upgrade rewrites the vendored `.github/workflows/llz-*.yml` bodies. Those
are in `terraform.yml`'s `pull_request` `paths:` filter, so the upgrade PR selects
the Terraform pipeline — including `plan-cluster-pr`, whose `llz ci tf-import` step
**writes** `cluster/<deployment>/terraform.tfstate` with nothing serialising it
against a concurrent apply.

`plan-cluster-pr` skips **draft** pull requests, and that skip exists precisely so
an automated one cannot take that write. So the bot opens a draft:

| Check | On the draft |
|---|---|
| **repo-readiness** — the newly mandatory secret this release needs | **runs** (it does not skip drafts) |
| lint | runs |
| `plan-cluster-pr` → `llz ci tf-import` | **skipped** |

`terraform.yml` lists `ready_for_review` in its trigger types, so a human who wants
the plan marks the PR ready and gets it — with a person watching, which is the
condition the unserialized write was always safe under.

**An earlier draft of this workflow opened a non-draft PR and claimed the hazard
was avoided.** It was avoided only for a pin-only upgrade, which selects nothing;
every real upgrade walked straight into it. The one-word fix is the whole
mitigation, which is why it has its own test asserting the flag on the real argv
rather than restating it.

**The residual gap** is a pin-ONLY upgrade, which selects no workflow and so gets
no repo-readiness. It is checked at the next PR touching Terraform or CI. That
trade-off is pre-existing and deliberate — see `terraform.yml`'s own filter
comment — and this workflow does not touch the filter to close it. If it is worth
closing later, the cheap way is inside `llz ci tf-import`: refuse to write when
`GITHUB_EVENT_NAME=pull_request`. No workflow change, and an older `TF_IMAGE`
keeps today's behaviour rather than failing on something it does not have.

## Why it ships disarmed

`LLZ_TEMPLATE_UPGRADE` defaults unset. An
adopter opting into a bot that opens PRs against their infrastructure repo is a
decision, not an inheritance.

## What stops it opening the same thing twice

The interlock is **two questions about pull requests**, not about refs — and that
is the correction to three earlier attempts that all keyed on the branch name:

| Question | If yes |
|---|---|
| Is an upgrade pull request from any earlier run still **open**? | leave it; do not stack a second behind it |
| Was a pull request for **this version** closed unmerged, and never merged since? | leave it; a reviewer rejected this upgrade |

Anything else opens a pull request. Both questions key on the branch **name**
rather than on a label: `gh pr create` drops the label on a 422 retry, and a guard
that depends on something optional goes missing exactly when the forge is having a
bad day.

The two match the name differently, and the difference is load-bearing. The first
takes any branch under the **stem** `chore/template-upgrade-`, because it asks
about upgrades in general. The second must match the version **exactly**
(`ProposesVersion`): the stem for `v1.2.3` is also a prefix of
`chore/template-upgrade-v1.2.3-rc1-<sha>`, so a stem match let one closed
pre-release refuse the GA release of the same number forever, at exit 0 on a green
run. `+build` metadata collided the same way, since `sanitizeRef` maps `+` to a
dash. What `BranchName` appends is a short SHA and nothing else, so anything left
over that is not one belongs to a different version.

`--state closed` returns **MERGED** rows as well as closed ones, and they mean
opposite things — so the whole page is read and merged wins. A version can carry
both (a first attempt closed, a later one merged), and it is then in the tree;
stopping at the first closed row refused every later legitimate re-commit at that
pin — which is what a `managed` file restored from drift is — permanently.

**The branch name carries the commit SHA and therefore cannot collide.** That one
property removed a surprising amount of machinery — orphan-branch recovery, a
`--force-with-lease` push, its hidden dependency on `fetch-depth: 0`, and a
"spent branch" case — because none of those problems exist once two runs can never
compute the same ref.

It took three tries to get here, and the failures are worth recording because each
looked like a fix:

- **`--state open`** could not see a pull request a reviewer had **closed**, so a
  rejected upgrade was force-pushed and reopened every month.
- **`--state all`** could not see that a **merged** one is spent. `llz upgrade`
  also restores drifted `managed` files, so it can legitimately commit again at an
  unchanged pin — and that work was discarded monthly behind a green run.
- **Renaming only the merged case** produced branches no later run ever queried, so
  duplicates stacked and the "leave a reviewer's diff alone" guard became
  structurally unreachable for every branch that path created.

Each of those is the same mistake: making a git ref carry a decision about intent.

A `gh pr create` that fails after a successful push leaves a branch with no pull
request. Nothing recovers it, and nothing needs to — the next run proposes again on
a new name, so the work is never stranded and the stale ref is inert.

## Two things the YAML has to work around

- **`copier` had to be added to `ci-tofu`.** It was installed only in the
  `devcontainer` stage, on the reasonable assumption that `llz upgrade` is always
  an operator's local command. `copier.Require` is a hard prereq, so without it
  this job fails at its first real step. It is pinned to the same `>=9.4,<10`
  range as the devcontainer so CI and local upgrades render through the same
  copier major.
- **No backticked CLI commands inside `run:` blocks.**
  `TestDeliveredWorkflowCommands` extracts every `run:` script and resolves each
  CLI invocation against the real cobra tree. It strips neither shell comments nor
  heredoc prose, and a command wrapped in backticks or quotes tokenises with the
  closing character attached to the verb — so documentation reds the gate.
  Commands go in fenced blocks, which tokenise clean.

## What is unproven

The release-e2e lane does not drive this workflow, and cannot sensibly: it
upgrades an instance to the *latest published release*, while the lane's instance
is scaffolded from the commit under test. Running it there would either no-op or
drag the fixture onto a different version mid-run.

The `llz upgrade` it wraps is covered by `llz ci upgrade-test`, which performs a
real upgrade across the last three releases on every lint run — asserting
non-interactivity, preserved answers, an advanced pin, and no conflict markers.
