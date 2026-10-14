---
name: release
description: Cutting an LLZ release, and the pin/immutability traps around it. Use when asked to cut, tag, promote or roll back a release; when a chart change needs to ship; when an adopter's pin or CI image looks skewed; or when pre-merge e2e needs artifacts that only exist after a merge. User-invoked only — releasing is a human decision, and the promote click is the approval.
disable-model-invocation: true
---

# Cutting a release

[`terraform-modules/RELEASING.md`](../../../terraform-modules/RELEASING.md) is
canonical for the umbrella tag and
[`kubernetes-charts/README.md`](../../../kubernetes-charts/README.md) for charts.
This file is the operating procedure and the traps, because publishing here is
close to a one-way door: **tags are immutable and charts are immutable**.

> **Never promote a release yourself.** Step 2 below is the human approval. Do
> everything up to it, then hand it over.

## The shape

One bare SemVer tag `vX.Y.Z` versions the modules, the reusable workflows, the
`llz` binaries and the `firewall-controller` image at the same commit. **Helm
charts are the exception** — independently versioned via each `Chart.yaml`, and
published on merge to main, not by the release.

There is **nothing to bump first**. The template hardcodes no version: the pin
lives once in `.copier-answers.yml` and the CLI is the version anchor. If you find
yourself writing a version literal into the delivered surface, stop — the
upgrade-churn guard will reject it, and it would add a line to every instance's
upgrade diff on every release forever.

Per ADR 0003 the workflow bodies are referenced by repo-local `./` paths, so
there is no cross-repo `uses:@vX.Y.Z` to pin and no `template-ref:` input — CI
reads the pin from `.copier-answers.yml` at runtime.

## Step 0 — pre-flight

- Working tree clean on the release commit.
- `make LINT_ALL=1 lint` exits 0 (the unconditional sweep, not the change-aware one).
- `make coverage` green.
- **Pick the version off the INTERFACE.** MAJOR = a breaking change to any module
  input/output, reusable-workflow input/secret, or the scaffold file contract;
  MINOR = a backward-compatible addition; PATCH = a fix with no interface change.
  The module READMEs' Inputs/Outputs tables and each reusable workflow's
  `on.workflow_call` are the SemVer surface — diff those to decide.

## Step 1 — pre-release, which arms the gate

```bash
gh release create vX.Y.Z --prerelease --generate-notes
```

That creates the tag and fires `release: prereleased` → `release-e2e.yml` stands
up a real LKE-E cluster (slow, billable). The pre-release is **not consumable**:
`llz self-update` and `llz new` skip pre-releases, and no binaries or image are
built yet.

While it runs, the failure surface is the `e2e-triage` skill.

## Step 2 — promote, once e2e is green (human)

Unchecking "pre-release" fires `release: released`, which builds the CLI binaries
and the `firewall-controller` image. **That click is the approval** — e2e cannot
mechanically block it.

If e2e fails: **fix forward with the next patch version.** Do not promote a red
tag and do not move a tag. A failed run leaves only an immutable tag and a
pre-release object, both ignored by the CLI — nothing public.

A **direct full release skips the gate entirely.** Always pre-release first.

## The traps

### Pre-merge e2e uses PUBLISHED artifacts

`release-e2e` pins source to a branch SHA but consumes the published CI image and
OCI charts, which are built **on merge to main**. So a branch whose fix lives in a
chart or the image will run e2e against the *old* artifact and fail for a reason
that is not in the diff. Publish the branch's artifacts first, or expect a
misleading red.

### Charts are immutable by version

`publish-charts.yml` **skips** any chart whose `Chart.yaml` `version:` is already
published. A NetworkPolicy fix without a version bump silently never ships — no
error, no warning, the old chart keeps serving.

`chart-version-guard` fires on **any** edit inside a chart directory, including a
README; `chart-pin-guard` asserts every Argo pin matches a local chart version.
And the bump **cascades**: `llz-argo-bootstrap-apps` pins the other charts, an
unmoved pin 404s at Argo sync time, so moving the pin bumps that chart too. Do not
weaken either gate in the PR that trips it. `helm lint --strict` + `helm template`
must be clean (`make helm-lint-charts`).

### The two pin records must agree

`.copier-answers.yml` carries the version twice — `llz_version` (the answer) and
`_commit` (copier's record of the merged template state). Every reader prefers
`llz_version`, so a `copier update` that does not apply leaves an instance
*deploying* the new release's shared manifests from the old release's scaffold:
individually well-formed, no parse error, and it happened on a live instance.
`llz lint` and `llz ci assert-image-fresh` fail on the mismatch; the fix is to
re-run the upgrade so copier rewrites both.

### The adopter's pin is not the shape e2e tests

Every e2e run pins the throwaway instance to one commit across all three legs, by
construction — **the one configuration in which the image pin cannot be wrong.**
An adopter is pinned at a release *tag* with an image computed by `llz tokens`.
That divergence shipped: a mutable image tag meant a release-pinned tree ran
main's `llz`, and the adopter's first pipeline died on render drift they could not
resolve. Green e2e throughout.

`llz ci assert-adopter-pin` now gates this at candidate time — the pin resolves,
`llz tokens` computes an **immutable** pin naming that same commit, both images
are actually **published**, and `assert-image-fresh` accepts a binary stamped
there while rejecting one stamped elsewhere.

> A release whose commit never got a successful `build-images` run hands every new
> adopter an unpullable image. This is why `build-images` runs on **every** push to
> main rather than behind a path filter.

> **`llz tokens` will not correct an image pin that is already set.** It computes
> `TF_IMAGE`/`KUBE_IMAGE` only when they are absent, deliberately, so it never
> overwrites an operator's choice. An instance carrying a stale pin from before
> this fix has to have those variables deleted (or corrected) by hand — upgrading
> alone will not do it.

## After the release

An adopter takes it with `llz self-update`, then `llz upgrade`. Do not add a
version-bump step for the first-party pins — Renovate is deliberately disabled on
them so `llz upgrade` stays the single channel; Renovate PRs move charts and
external actions only.

If the version bump produced a diff larger than a handful of lines in a delivered
instance, that is a churn regression, not a normal release — see the
`delivered-surface` skill.
