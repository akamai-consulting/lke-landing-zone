# `llz-discover-deployments` — maintainer rationale

`instance-template/.github/workflows/llz-discover-deployments.yml` is a tiny
reusable (`workflow_call`) shim that is the **single source of truth for the
per-deployment CI matrix**. It is **vendored verbatim into every customer
instance by copier**, so it runs self-contained with no cross-repo checkout. See
`docs/adr/0003-vendor-actions-and-bodies-into-instances.md` for the
surface-reduction pattern.

Because the YAML is copied into instances where it can never be updated in
place, long-form maintainer archaeology lives here in the template repo instead
of in the workflow body. This document is the archive; the inline comments are
the 3am debugging aids.

---

## Why one shared shim

`llz env list --json` emits the deployment set — from the LandingZone spec
(`environments/<env>.yaml`) on spec-driven instances, unioned with any committed
`cluster/<name>.tfvars` on legacy instances. (The per-env tfvars are gitignored
build artifacts on spec instances, so this reads the spec, not them.)

Three other workflows fan their matrices out over **this** workflow's
`deployments` output: the scheduled health/audit checks, the credential
rotation, and the Terraform PR plan. Because every matrix derives from the same
call, they cannot drift from one another — or from Terraform. A rotation run can
never leave a checked-but-unrotated (or rotated-but-unchecked) deployment, and
`llz env add` extends CI coverage everywhere at once.

## Read-only by construction

It only reads the CALLER's checked-out spec/tfvars — no secrets, no cluster
access. `llz` is baked into `vars.TF_IMAGE` (resolved from the caller's repo/org
variables); the CI image is pulled with the built-in `GITHUB_TOKEN`
(`github.actor`), so the image must be public or grant the caller repo
package-read access.

## Event gating is the caller's job

The caller owns the event gating: it puts its own `if:` on the job that `uses:`
this workflow (e.g. schedule/dispatch-only, or same-repo-PR-only). A skipped
discover skips its dependent matrix jobs by default (skipped `needs`
propagate), which is the intended behaviour off the gated events.

## The `deployments` output shape

A sorted JSON array of deployment names (one per
`terraform-iac-bootstrap/cluster/<name>.tfvars`), e.g. `["itest"]` or
`["primary","secondary"]`. It emits `[]` (not `null`) when none exist, so a
`fromJSON()`-fed matrix degrades to an empty fan-out rather than failing.

## Why `defaults.run.shell: bash`

The List-deployments step runs inside the `ci-terraform` container and uses
`set -o pipefail` (a bashism). Without this default, GitHub falls back to the
container's `/bin/sh` (dash), which rejects `-o pipefail` ("Illegal option") and
fails the job on **every** scheduled run — taking auto-unseal, scheduled-checks
and secret-rotation down with it. The invariant is enforced repo-wide by
`llz ci check-workflow-shells`.
