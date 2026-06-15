# Releasing the landing zone

This is the versioning + distribution contract for the whole landing zone, per
[docs/templatization-plan.md](../docs/templatization-plan.md) §6, §9 (Phase 3).
The Terraform modules under `terraform-modules/` are published as versioned
`git::` sources, and the monorepo's own roots consume the tagged versions — the
dogfood relationship that keeps the extraction honest.

## One umbrella tag per release

The whole landing zone versions in **lockstep under a single bare SemVer tag
`vX.Y.Z`** (no prefix). That one tag versions every first-party artifact at the
same commit:

- the Terraform modules (`terraform-modules/*`), consumed as `git::?ref=vX.Y.Z`;
- the reusable workflows (`.github/workflows/llz-*.yml`) + their composite
  actions and the `llz` CLI built from the template checkout, consumed by an
  instance as `uses: …@vX.Y.Z` + `template-ref: vX.Y.Z`;
- the `llz` CLI release binaries (attached to the release by `llz-release.yml`);
- the `firewall-controller` image (tagged `:vX.Y.Z` by `firewall-controller.yml`).

> **Why one tag, not per-module tags.** Earlier this repo used per-module tags
> (`llz-pool/vX.Y.Z`) to allow modules to version independently. In practice every
> instance pins them in lockstep, so independent versions were pure overhead.
> Terraform's `?ref=` accepts any git ref (the `//…/llz-cluster` path selects the
> module subdir), so a bare `?ref=vX.Y.Z` resolves a module just as well. One tag
> means one thing to cut and one number to reason about.

The **Helm charts are the exception**: they version independently via each
`Chart.yaml` `version:` and publish immutably from `publish-charts.yml` on merge
to main (bump `version:` to release). They are not part of the umbrella tag; an
Argo `targetRevision:` chart pin is left untouched by the release flow.

SemVer applies to the umbrella interface: **MAJOR** = a breaking change to any
module input/output, reusable-workflow input/secret, or the scaffold file
contract; **MINOR** = a backward-compatible addition; **PATCH** = a fix with no
interface change. Each module's README Inputs/Outputs tables and each reusable
workflow's `on.workflow_call` are the SemVer surface.

Tags are **immutable** — never move a tag. To release a change, cut a new one.

## Distribution: tagged `git::` sources

Modules are consumed straight from this repo over SSH, pinned to the umbrella tag:

```hcl
module "node_pool" {
  source = "git::ssh://git@github.com/akamai-consulting/lke-landing-zone.git//terraform-modules/<name>?ref=vX.Y.Z"
  # …inputs…
}
```

- The `//…` segment is the module's path **within** the repo; `?ref=` pins the
  umbrella tag. Terraform shallow-clones the repo at that tag and roots the module
  at the subdirectory.
- A future graduation step (more system teams adopting) is a private Terraform
  Registry; the tag contract carries over.

## Cutting a release

The real-cluster e2e is the gate, and a release goes public in **two human
steps** — pre-release to validate, then promote to ship. There is nothing to bump
first; the template carries no version literals (see "How instances pin" below).

**Step 1 — publish a pre-release `vX.Y.Z`** on github.com (GitHub UI → check "Set
as a pre-release", or `gh release create vX.Y.Z --prerelease --generate-notes`).
This creates the tag and fires `release: prereleased`:

| Workflow | On `prereleased` | Result |
| --- | --- | --- |
| `release-e2e.yml` | stands up a real LKE-E cluster | the gate (create → validate → destroy) |

The pre-release is **not** consumable: `llz self-update`/`new` skip pre-releases
(`selfupdate.go`), so the tag exists but no adopter picks it up, and the binaries
and image are **not** built yet.

**Step 2 — promote to a full release** once e2e is green: edit the release and
uncheck "pre-release" (or `gh release edit vX.Y.Z --prerelease=false --latest`).
That fires `release: released` — the human promotion **is the approval click**:

| Workflow | On `released` | Result |
| --- | --- | --- |
| `llz-release.yml` | builds the CLI binaries | attaches `llz-<os>-<arch>` + `SHA256SUMS` |
| `firewall-controller.yml` | builds the operator image | pushes `ghcr.io/<owner>/firewall-controller:vX.Y.Z` |

> **Why the split.** GitHub fires no workflow when a *draft* is saved, and a
> release published with the built-in `GITHUB_TOKEN` suppresses downstream runs —
> so a human publishing the pre-release arms e2e, and a human promoting it (after
> green) arms the binaries/image. e2e can't *mechanically* block promotion (the
> gate is convention + the promote click), but a failed run leaves only an
> immutable tag and a pre-release object — both ignored by the CLI, nothing public.
> If e2e fails, fix forward with `vX.Y.Z+1`; don't promote `vX.Y.Z`.
>
> A **direct** full release (skipping the pre-release) fires `released` straight
> away and bypasses the gate — always pre-release first. To *enforce* it, restrict
> creation of `v*` tags to a release bot via a repo ruleset and let only an
> automated, e2e-gated workflow create them.

## How instances pin — the CLI is the version anchor

The template **never hardcodes a version**. `instance-template/`'s first-party
references are copier placeholders — `?ref=<@ llz_version @>`,
`uses:@<@ llz_version @>`, `template-ref: <@ llz_version @>`. The concrete version
is injected at scaffold/upgrade time by `llz`:

- `llz new` runs `copier copy --vcs-ref <ref> --data llz_version=<ref>`, and
- `llz upgrade` runs `copier update --vcs-ref <ref> --data llz_version=<ref>`,

where `<ref>` defaults to the **`llz` binary's own version** (`llz --version`),
or an explicit `--ref`. So installing `llz vX.Y.Z` and scaffolding/upgrading pins
the instance to `vX.Y.Z` artifacts — the CLI is the single version anchor, and the
rendered instance is still fully pinned (`?ref=vX.Y.Z`), reproducible, and
upgraded as a reviewable diff.

This is why there is **no pin-bump PR and no chicken-and-egg**: cutting tag
`vX.Y.Z` needs no pre-committed pin, and `llz new --ref vX.Y.Z` renders
`?ref=vX.Y.Z` — which resolves because you just cut it. An adopter takes a new
release by `llz self-update` then `llz upgrade` (or `-d llz_version=vX.Y.Z` for a
manual `copier`). Charts are the exception — independently versioned and
Renovate-managed in the instance.

## Internal module-to-module references

`llz-cluster` calls `llz-node-firewall` via a **relative** source
(`../llz-node-firewall`). This is intentional and works under `git::` consumption:
Terraform checks out the whole repo for the `git::` fetch, so the relative path
resolves to the sibling module inside that same checkout — pinned to the *same*
umbrella tag as the parent. Do **not** rewrite internal references to `git::`;
that would let the two halves drift to different versions.

## Module interface = the published contract

Each module ships a `README.md` documenting inputs/outputs and a `versions.tf`
pinning `required_version` + provider constraints. Treat the README's Inputs/
Outputs tables as the SemVer surface — changing them is what drives the version
bump.
