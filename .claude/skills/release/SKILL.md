---
name: release
description: Guide the two-step, e2e-gated umbrella release of the landing zone (pre-release tag, release-e2e, promote) and the independent Helm chart version-bump flow. User-invoked only - releasing is a human decision.
disable-model-invocation: true
---

# Release the landing zone

Read `terraform-modules/RELEASING.md` (canonical for the umbrella tag) and
`kubernetes-charts/README.md` (canonical for charts) IN FULL before acting.
This skill is the checklist, not the authority.

## Umbrella release (`vX.Y.Z`) — two human steps, gated by e2e

Everything versions in lockstep under one bare SemVer tag: Terraform modules
(`git::?ref=vX.Y.Z`), reusable workflows + scaffold (`uses:@vX.Y.Z` /
`template-ref:`), the `llz` binaries, and the `firewall-controller` image.
There is nothing to bump first — the template hardcodes no version (`llz` is
the version anchor; copier renders `<@ llz_version @>` pins from it).

1. **Pre-flight**: working tree clean on the release commit;
   `make LINT_ALL=1 lint` exits 0; `make coverage` green.
2. **Pick the version**: SemVer on the INTERFACE — MAJOR = breaking module-IO /
   reusable-workflow-input / scaffold change, MINOR = backward-compatible
   addition, PATCH = fix. The module READMEs and the workflows'
   `on.workflow_call` blocks are the SemVer surface; diff those to decide.
3. **Step 1 — publish a PRE-RELEASE** `vX.Y.Z`. This fires
   `release: prereleased` → `release-e2e.yml` stands up a REAL cluster
   (slow, billable). Pre-release tags are invisible to `llz self-update`/`new`;
   no binaries or images are built yet.
4. **Wait for e2e green.** If it fails: fix, then cut a NEW tag — **tags are
   immutable, never move one**. (For failure triage, use the `triage-e2e`
   skill.)
5. **Step 2 — PROMOTE** the pre-release to a full release (uncheck
   pre-release). This fires `release: released` → `llz-release.yml` (binaries)
   + the firewall-controller image build. The promote click IS the approval;
   nothing public exists until it.

## Helm charts — the independent track

Charts version via `Chart.yaml` `version:` only, published to GHCR as OCI
(`oci://ghcr.io/<org>/charts/<chart>:<version>`), immutable by convention:
`publish-charts.yml` skips any already-published version.

- To release a chart change: **bump `version:` in its `Chart.yaml`** — never
  overwrite an existing version (Argo Applications pin `targetRevision`).
- `chart-version-guard` (CI) fails a PR that changes a chart without a bump;
  `chart-pin-guard` asserts every Argo pin matches a local chart version.
- `helm lint --strict` + `helm template` must be clean (`make helm-lint-charts`).

## Hard rules

- Tags are immutable. To release a change, cut a new tag.
- Do not add a version-bump step for the first-party pins — Renovate is
  deliberately disabled on them; `llz upgrade` is the single channel.
- Never publish a full release that skipped the pre-release e2e gate.
