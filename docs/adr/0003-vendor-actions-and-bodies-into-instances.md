# ADR 0003 — Vendor the composite actions (and reusable workflow bodies) into every instance

- Status: Accepted
- Date: 2026-07-16
- Deciders: platform / LLZ maintainers
- Related:
  - `docs/designs/cross-org-reuse-pattern.md` (the boundary this removes)
  - `tools/cmd/llz/doctor_crossorg.go` — the `llz doctor` cross-org guardrail (#200)
  - `instance-template/.github/actions/**`, `instance-template/.github/workflows/llz-*.yml`
  - ADR 0002 — Thin Terraform
  - PR #16 — `internal/forge` + `forge_flavor` (the GHE / GitLab portability track this unblocks)

## Context

The CI pipeline is distributed to instances as thin caller workflows that delegate
to reusable workflow **bodies** and **composite actions** owned by the template
repo. Those were consumed **cross-repo**:

- reusable bodies referenced as
  `uses: <upstream_org>/lke-landing-zone/.github/workflows/llz-*.yml@<ref>` +
  `secrets: inherit`;
- composite actions referenced cross-org, with `cluster-access` **self-checking-out**
  the template (org hardcoded, `template-ref` input) to reach its sibling actions,
  because a composite `uses: ./<path>` resolves against the *caller's* workspace, not
  the action's own repo.

Two GitHub properties make cross-repo consumption fail for an instance in a
different org — or on an isolated GitHub Enterprise:

1. **`secrets: inherit` does not cross an organization boundary.** For an instance in
   org A calling a reusable in org B, every inherited secret (repo-, org-, and
   environment-scoped) arrives **empty**; the pipeline runs with no credentials and
   fails downstream with cryptic errors (`No valid credential sources found`,
   `require-secret … is not set`). Confirmed live: `akamai/gsap-apl` →
   `akamai-consulting/lke-landing-zone`. The `llz doctor` cross-org guardrail (#200)
   exists to catch exactly this at preflight.

2. **Resolving a `uses:` that points at another repo requires that repo to be
   reachable at runtime** — whether it is a reusable workflow body, a composite
   action, or `cluster-access`'s self-checkout of the template. GitHub Connect only
   resolves *public github.com* actions; a private repo, or an air-gapped GHE with no
   route to github.com, is not reachable and must be **mirrored**. Mirroring the
   central repo is precisely the operational burden a GHE adopter needs to avoid.

The intermediate "serve bodies from the template, self-checkout the actions" model
kept the actions/bodies central. It still requires the central repo to be reachable
and still hardcodes one org (`cluster-access` is never copier-rendered, so the org
cannot be a token) — so it does not work on an isolated GHE. Same-org/enterprise
secret inheritance is also an org-admin dependency outside the template's control and
is no help for a genuinely separate adopter org.

## Decision

**Vendor the reusable workflow bodies AND the composite actions into every instance,
and reference every one of them with a repo-local `./` path.** A rendered instance is
**self-contained**: at runtime it depends on nothing outside itself except the handful
of standard Marketplace actions (`actions/checkout`, `actions/upload-artifact`) an
enterprise mirrors regardless.

Concretely:

- The 7 reusable bodies live in `instance-template/.github/workflows/llz-*.yml`; caller
  stubs invoke them as `uses: ./.github/workflows/llz-*.yml` + `secrets: inherit`.
  Because the reusable is **same-repo**, inheritance resolves and each env-declaring
  job reads its own instance/environment secrets directly.
- The 5 composite actions + `_lib/` are vendored under
  `instance-template/.github/actions/`; all body references become
  `./.github/actions/<name>`. `cluster-access` **drops its template self-checkout and
  its `template-ref` input** — its siblings resolve locally from the vendored tree (a
  composite `uses: ./<path>` resolves against the caller's workspace, which for an
  instance job IS the instance repo that now carries `.github/actions/`).
- copier delivers the vendored trees; `.template-manifest` classifies
  `.github/actions/**` as `managed`; `.template-removals` no longer strips the bodies.
  The actions are token-free, so they render byte-identical.

This makes the cross-org guardrail (#200) pass **by construction**: every `uses:` is a
local `./` ref (`usesOrg == ""` → skipped), so there is no cross-org boundary left.

## Consequences

**Positive**
- **Cross-org templates and instances work.** An instance in any org, or on an
  isolated GHE, runs with no cross-repo fetch and no `secrets: inherit` boundary — no
  mirror of the central repo, no GitHub Connect dependency.
- The portability seam becomes simply *"the instance carries its own pipeline,"* which
  composes cleanly with the forge abstraction (PR #16) for GHE / GitLab flavors.
- Verified end-to-end: `make instance-test` (copier render + tofu validate + actionlint
  on the rendered instance), `llz ci template-manifest`, and `llz doctor` — whose
  "workflow reuse" section is now green on a rendered instance.

**Costs / trade-offs**
- Instances carry more vendored surface (bodies + actions). It is kept current on the
  same `llz upgrade` path as the rest of the scaffold — real bytes and a real sync
  contract, but no manual drift. The bodies ship **verbatim** (comments included):
  `.github/workflows/**` is `merge`-classified, and copier's 3-way merge on
  `copier update` uses the template render as its base — a render-time comment-strip
  would make every instance copy diverge from that base and turn each release that
  touches a comment into merge conflicts inside workflow YAML.
- A job that genuinely needs template **source** (e.g. the former scheduled-checks
  `go-vuln-audit`, which built the template's Go module) can no longer run from an
  instance; that concern moves to the template repo's own CI
  (`.github/workflows/go-vuln-audit.yml`, same weekly cron).

## Alternatives considered

- **Central reusables/actions referenced cross-repo** (the prior model, incl.
  `cluster-access` self-checkout). Fails on cross-org secret inheritance AND on
  air-gapped GHE reachability, and hardcodes one org. Rejected.
- **Keep bodies central but reference composite actions cross-org at a pinned tag.**
  Still a private-repo fetch a GHE instance must mirror; and the child ref cannot be
  templated inside an un-rendered action, so nested composites cannot track the
  caller's version. Rejected.
- **Rely on same-org / same-enterprise secret inheritance.** Works only inside one
  org/enterprise, is an org-admin dependency outside the template's control, and is no
  help for a separate adopter org. Rejected as the general answer.
- **Per-adopter fork of the template.** Per-org maintenance, two refs to keep in
  lockstep, and silent drift. Rejected — it reintroduces the burden the platform
  exists to remove.
