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

## The pull request opens as a DRAFT

A genuine upgrade rewrites the vendored `.github/workflows/llz-*.yml` bodies, which
are in `terraform.yml`'s `pull_request` `paths:` filter, so the upgrade PR selects
the Terraform pipeline.

**The draft used to be the mitigation, and is now hygiene.** The pipeline carried a
`plan-cluster-pr` job whose `llz ci tf-import` step **wrote**
`cluster/<deployment>/terraform.tfstate` with nothing serialising it against a
concurrent apply. That job skipped draft pull requests, so opening a draft was what
kept an automated PR from taking the write — a one-word mitigation with its own test
asserting the flag on the real argv rather than restating it.

That job is retired. It could never resolve the environment-scoped credentials it
needed (`TF_STATE_ACCESS_KEY`, `TF_STATE_SECRET_KEY`, `LINODE_API_TOKEN`), and it
could not be given the `infra-<deployment>` environment holding them either, because
`llz` locks that environment to `ref=main` — a pull request's ref is
`refs/pull/N/merge`, which no branch policy matches. Nothing on a pull-request path
writes Terraform state any more, and two gates keep it that way:
`llz ci workflow-secret-scope` and `TestNoPullRequestPathWritesTerraformState`.

What the upgrade PR gets today, draft or not:

| Check | On the PR |
|---|---|
| **repo-readiness** — the newly mandatory secret this release needs | runs |
| tf-lint, checkov, promote-pipeline-drift | run |
| any job that writes Terraform state | **none exists on this path** |

The `--draft` flag stays because a bot PR has no business asking for review, not
because anything now depends on it.

**The pin-only gap is closed.** An upgrade whose only change was the recorded pin
used to select no workflow and so get no `repo-readiness` — the v0.0.42 /
`TF_STATE_ENCRYPTION_PASSPHRASE` case. `.copier-answers.yml` was deliberately kept
out of `terraform.yml`'s filter because a `paths:` filter selects the *workflow*,
not a job, and listing it would have handed the state write to every automated
pin bump. With no state write left on the path, the pin is in the filter and a
pin-only upgrade PR reaches `repo-readiness` like any other.

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
