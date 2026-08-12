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

`--ref` skips the self-update, because an explicit target needs no resolution.

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
[../secrets.md](../secrets.md) for its scopes; note its lapse is silent, because
a monthly workflow that stops running looks like an instance with no upstream
changes.

## What made this safe to build: the `tf-import` gate

An automated, non-draft PR against `terraform.yml`'s trigger paths used to be
actively dangerous, and the danger is worth restating because it is not obvious.

`plan-cluster-pr` runs `llz ci tf-import`, which **writes**
`cluster/<deployment>/terraform.tfstate`. Nothing serialises that against a
concurrent apply: the PR job's concurrency group is `terraform-infra-pr` while a
dispatched apply's is `terraform-infra-<deployment>`, and the S3 backend sets no
`use_lockfile`. Two writers, last one wins. The draft-PR skip exists so a human
can opt out of that write; a bot PR is not a draft.

The import step is now gated on a `changed-paths` job, so it runs only for a PR
whose diff touches `terraform-iac-bootstrap/`, `landingzone.yaml` or
`environments/`. An upgrade PR rewrites `llz-*.yml` and the pin, gets its plan and
its repo-readiness, and writes no state.

Two consequences worth knowing:

- **The template pin came back into `terraform.yml`'s `paths:` filter.** It had
  been removed precisely because a `paths:` filter selects the *workflow*, not a
  job, so listing the pin also armed the state-writing plan job. With the write
  gated, that is no longer true.
- **The two halves are coupled by a test.** `TestPinInTriggerImpliesImportIsPathGated`
  fails if either moves without the other, in both directions, because pin-listed
  + import-ungated silently restores the hazard and nothing else would notice.

## Why it ships disarmed

Same reasoning as `llz-scheduled-apply`: `LLZ_TEMPLATE_UPGRADE` defaults unset. An
adopter opting into a bot that opens PRs against their infrastructure repo is a
decision, not an inheritance.

## Idempotency and the branch name

The branch carries the target version (`chore/template-upgrade-<version>`), and
the job checks the remote for it before pushing. So a second run while an earlier
upgrade PR is still open leaves it alone rather than force-pushing over an
unreviewed diff, and a *later* release opens its own PR rather than silently
retargeting the old one.

`llz upgrade` exits 0 both when it upgraded and when the instance was already
current, so the commit — HEAD moving — is the only honest signal that there is
anything to push.

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
