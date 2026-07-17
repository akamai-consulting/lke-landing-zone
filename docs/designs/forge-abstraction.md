# Design: the forge abstraction — GitHub.com, GHEC, GHES, GitLab

**Status:** Design only (no code). Nothing here has landed. `spec.instance.forge`
already exists in [validate.go](../../tools/internal/validate/validate.go) and
already accepts `gitlab`, but it is **dead** — validated and never branched on
(§Problem). This doc either makes that field real or deletes it; it should not
survive another release as aspirational residue. Sequencing is deliberately
front-loaded on Phases 0–1, which are cheap, fix live bugs, and deliver GHEC nearly
for free.
**Relates to:** [credential-single-pane.md](credential-single-pane.md),
[credential-single-pane-incluster.md](credential-single-pane-incluster.md),
[cross-org-reuse-pattern.md](cross-org-reuse-pattern.md),
[instance-slimming.md](instance-slimming.md), [../secrets.md](../secrets.md),
[../adr/0003-vendor-actions-and-bodies-into-instances.md](../adr/0003-vendor-actions-and-bodies-into-instances.md),
`tools/internal/validate/validate.go`, `tools/cmd/llz/gh_secrets_native.go`,
`tools/cmd/llz/ci_openbao_configure.go`, `tools/internal/clusterspec/kustomize.go`.

> This is a design PR (no code). It exists because the obvious version of the change
> — "add a `forge` switch and branch on it" — walks into three non-obvious realities
> that need a decision first: the field it would branch on is already there and
> already lying; the GitHub coupling has **escaped the CI layer into the cluster**;
> and the four forges do not have the same *capabilities*, so a uniform interface
> would have to paper over the one place they genuinely differ — rotation.

## Problem

We want an instance to live on GitHub.com, GitHub Enterprise Cloud, GitHub
Enterprise Server, or GitLab. Today it can live on exactly one of those, and every
layer assumes it.

**There is no abstraction layer. There is worse than none — there is a fake one.**
`internal/validate` defines `ForgeGitHub`, `ForgeGitHubEnterprise`, and
`ForgeGitLab`; `validate.Forge()` checks them; `clusterspec/types.go` carries
`Instance.Forge`. The **only** caller is `clusterspec/validate.go`, and it only
validates. There are zero behavioral branches on the value. The docstrings compound
it — the `Forge` const block says the flavors *"mirror copier.yml's forge_flavor
choices and forge.Flavor() in internal/forge"*, and `Instance` says it *"mirrors
copier.yml's questions (upstream_org, instance_repo, forge_flavor, llz_version)"*.
`copier.yml` has no `forge_flavor` key. `tools/internal/forge` does not exist. ADR
0003 cites *"PR #16 — `internal/forge` + `forge_flavor`"* as a related track; that
PR is unmerged. **A spec that accepts `forge: gitlab` and silently ignores it is
worse than one that rejects it**, and it is what an adopter meets first.

The real coupling, by plane:

| Plane | Coupling | Escapes to |
|---|---|---|
| REST | `gh_secrets_native.go` — sealed-box `PUT` (`box.SealAnonymous`), `X-GitHub-Api-Version`, env-secret keyed by **numeric repo id** | CI + **cluster** |
| REST | `ci_gh_pat_expiry.go` — the `GitHub-Authentication-Token-Expiration` response header | CI + **cluster** |
| OIDC | `ci_openbao_configure.go` — issuer, audience, **and the `repository` bound-claim name** | CI + **cluster** |
| OIDC | `kyverno-verify-llz-image-signature.yaml` — sigstore issuer pin | **cluster (admission)** |
| git | `ci_bootstrap_cluster.go` / `authedGitURL` — the `x-access-token:{token}@` convention | CI |
| Argo | `ci_bootstrap_cluster_manifests.go` — hardcoded `https://github.com/` in `sourceRepos` + two Applications | **cluster** |
| Argo | `clusterspec/kustomize.go` / `remoteBasePrefix` — remote base as a **Go constant**, cloned **anonymously** | **cluster** |
| Net | `harbor-robot-provisioner/network-policy.yaml`, `broad-pat-rotator/network-policy.yaml` — egress written around api.github.com | **cluster** |
| Wizard | `wizard.go` — github.com deep links, fine-grained-PAT permission model | operator |
| CI | ~7,400 lines of Actions YAML; **only ~80 lines invoke `llz`** | CI |

**Consequence:** a forge abstraction confined to `llz`'s Go API layer is not
sufficient and will produce a demo that passes unit tests and wedges on a real
cluster. Admission control, NetworkPolicy egress, and Argo's repo-server all
independently assume GitHub. Any honest plan has to name the cluster-side coupling
as in-scope, not defer it to "integration."

Two live bugs fall out of the same survey, worth fixing regardless of this design:

- **`$GITHUB_API` is honored inconsistently.** `ci_gh_pat_expiry.go`,
  `ci_token_inventory.go`, and `token_validate.go` all read it;
  `gh_secrets_native.go` **ignores it** — its `ghAPIBase` comment calls the var a
  test seam. On a GHES instance the audit path and the write path would talk to
  *different servers*, and the audit would report green against the wrong one.
- **The wizard's minting URLs ignore `ghHost()`.** `wizard.go` resolves `GH_HOST`
  and its comment explicitly anticipates *"a GHE-hosted template fork"* — but every
  `ghTokenURL` / `ghFineGrained*URL` helper hardcodes `https://github.com/settings/…`
  anyway. A GHE operator is handed links to the wrong server today.

## Goals / non-goals

**Goals**

- One config axis — `spec.instance.forge` + a host — that drives API base, OIDC
  issuer/audience/claim-map, git credential convention, and Argo repoURLs.
- A real `tools/internal/forge` package, with capability interfaces rather than one
  fat `Forge`, so a forge that *cannot* do something says so at compile time.
- Token minting and rotation per forge, honest about the fact that **GitHub's floor
  is one permanent secret and GitLab's is zero** (§The rotation asymmetry).
- GitLab as a first-class forge, including orchestration on GitLab CI.
- Delete or resurrect `spec.instance.forge` — no third option.

**Non-goals**

- **Mirroring the template.** Settled: the template stays on github.com, public and
  anonymously readable, and is not mirrored per-adopter. This is load-bearing — see
  §The two-forge split, where it *removes* work rather than adding it.
- Air-gapped adopters. Settled: the cluster can reach github.com. If that ever
  changes, chart delivery becomes a new subsystem (OCI via Harbor) and this design
  does not cover it. Recorded in §Open questions as the assumption most likely to be
  challenged by a real enterprise adopter.
- Gitea/Forgejo/Bitbucket. The interface should not *preclude* them; nothing here is
  built for them.
- Retiring GitHub Actions for GitHub-family instances. Actions stays the native CI
  for three of the four forges.
- Changing what lives in OpenBao. The `secret/…` paths, ESO wiring, and k8s-auth
  roles are forge-independent and stay exactly as they are. Only the *driver* that
  mints and rotates changes.

## The two-forge split

The single most useful consequence of "the template is not mirrored" is that there
are **two** forge roles, not one, and only one of them varies:

```
UPSTREAM PLANE — always github.com, public, anonymous, read-only. NOT abstracted.
  │
  ├── akamai-consulting/lke-landing-zone       (template repo)
  │     ├── platform-apl/**                    → Argo kustomize remote ref
  │     │                                        clusterspec/kustomize.go
  │     └── platform-apl/manifest-secret-store → llz-secret-store Application
  │                                              ci_bootstrap_cluster_manifests.go
  ├── ghcr.io/akamai-consulting/llz:*          (llz image, sigstore-signed)
  └── .github/workflows/build-images.yml       (signs with GitHub OIDC)
        └── verified in-cluster by kyverno-verify-llz-image-signature.yaml
              issuer: token.actions.githubusercontent.com   ← CORRECT AS-IS

INSTANCE PLANE — customer's forge. github.com | GHEC | GHES | GitLab. ABSTRACTED.
  │
  ├── <host>/<org>/<instance-repo>             (values, TF, orchestration)
  │     ├── apl-<env> branch                   → apl-operator pushes here
  │     └── CI                                 → Actions or GitLab CI
  ├── CI secret plane                          (infra-<env> secrets / CI variables)
  └── CI OIDC                                  → OpenBao jwt role   ← VARIES
```

**The upstream plane needs no abstraction.** Charts, the `llz` image, and the
sigstore issuer are all artifacts of the *template's* CI, which runs on github.com
and will keep running there. That means the Kyverno image-signature policy's issuer
pin is **correct as written** and must **not** be made forge-aware — a GitLab-hosted
instance still runs a GitHub-signed `llz` image, and pinning the GitHub issuer is
precisely right. Likewise `kustomize.go`'s hardcoded org in `remoteBasePrefix`: with
no mirroring, the constant is not a bug, it is the invariant. Its comment already
says so — *"There are no private forks, so the org is a constant"* — and the
`?timeout=80` tuning beneath it documents the ~35MB anonymous clone that makes it
work.

This halves the problem, and it inverts the intuition: the deepest-looking coupling
in the codebase (a hostname baked into a Go constant, reached from admission control
and the repo-server) is the part we are **keeping**. The abstraction stops at the
instance plane.

The one thing this costs: **the template repo must stay public.** An anonymous clone
from the repo-server has no credential to present. If the template ever goes private,
`kustomize.go` needs a credential path and this section is wrong. Recorded in §Open
questions.

## The forge capability matrix

The four forges are not four skins on one API. They differ in kind, and the matrix
is the design — everything in §Proposed design falls out of it.

| | GitHub.com | GHEC | GHES | GitLab |
|---|---|---|---|---|
| API base | `api.github.com` | `api.github.com` (or `ghe.com` tenant) | `https://HOST/api/v3` | `https://HOST/api/v4` |
| OIDC issuer | `token.actions.githubusercontent.com` | same (+ optional enterprise slug) | **`https://HOST/_services/token`** | **the instance domain** |
| OIDC audience | `https://github.com/<owner>` | slug-customizable | instance-derived | instance domain; per-token via `id_tokens:` |
| Repo-identity claim | `repository` | `repository` | `repository` | **`project_path`** |
| `sub` format | `repo:o/r:ref:…` | same | same | `project_path:{g}/{p}:ref_type:{t}:ref:{b}` |
| Secret write | sealed-box `PUT` (NaCl) | same | same | **plaintext `POST`, `masked=true`** |
| Env-scoped secrets | `infra-<env>` | same | same | **no isomorph** (env-scoped variables are close, not equal) |
| Service credential | App installation token (1h) | same | App on the instance | project/group access token |
| Mint via API | yes (App JWT → install token) | yes | yes | yes |
| **Rotate root via API** | **no** | **no** | **no** | **yes** — `POST …/access_tokens/:id/rotate` |
| Expiry probe | `GitHub-Authentication-Token-Expiration` header | same | same | token-info API |
| Git credential | `x-access-token:{tok}@` | same | same | `oauth2:{tok}@` |
| CI system | Actions | Actions | Actions | GitLab CI |

Three rows carry almost all the work.

**`Repo-identity claim`** is the sleeper. Everyone expects the issuer to differ.
Fewer notice that GitLab's JWT has no `repository` claim at all — it has
`project_path`, `namespace_path`, `ref`, `ref_type`. So the OpenBao jwt-role body in
`ci_openbao_configure.go` is not "the issuer needs a variable"; **the whole role body
is forge-shaped**, and the abstraction has to produce the `bound_claims` object, not
a hostname.

**`Secret write`** deletes code rather than adding it. `gh_secrets_native.go`'s
`box.SealAnonymous` is a GitHub wire-format requirement; GitLab takes the value
plaintext over TLS with `masked=true`. The GitLab implementation is *smaller* than
the GitHub one. The `infra-<env>` environment-scope concept is the harder half —
GitLab's environment-scoped variables are similar but not isomorphic, and
`infra-<env>` is load-bearing across seven workflows plus `tokens.go` and
`regenroot.go`.

### The rotation asymmetry

This is the row a naive interface would paper over, and it is the reason
`TokenRotator` is a **separate interface** in §Proposed design rather than a method
that returns `ErrUnsupported`.

- **GitHub family (all three).** There is no API to create or rotate a GitHub App
  private key — it is a web-UI-only flow, on GHES exactly as on github.com. The best
  reachable state is: one **permanent** App private key, held in escrow or in
  OpenBao, minting 1-hour installation tokens. The floor is **one permanent secret**.
  What that buys over today's PATs is not automation — it is the removal of the
  *expiry cliff* (App keys never expire, so no 90-day hard-fail), the removal of
  human-account coupling, and an ephemeral effective credential. Up to 25 keys may
  coexist, so rotation is zero-downtime but human-initiated.
- **GitLab.** Project and group access tokens have a rotate endpoint, and the
  `self_rotate` scope lets a token rotate *itself*. The floor is **zero permanent
  secrets**: the in-cluster rotator can hold a token that renews its own lifetime on
  a schedule, with no escrowed root key anywhere.

**Consequence:** the forge abstraction cannot make rotation uniform, and should not
pretend to. It must expose rotation as a capability that some forges have and some do
not — and the *policy* around it (escrow, lead-time alerting, runbooks) legitimately
differs per forge. A design that hides this behind a uniform `Rotate()` would force
the GitLab implementation to be as bad as the GitHub one, which is exactly backwards.

## Proposed design

### `tools/internal/forge` — capability interfaces, not one fat `Forge`

Small interfaces, composed; capability probed by type assertion. A forge that cannot
rotate does not implement `TokenRotator`, and the compiler says so.

```go
package forge

type Flavor string // "github" | "github-enterprise" | "github-enterprise-server" | "gitlab"

// Forge is the always-present core. Everything else is probed.
type Forge interface {
    Flavor() Flavor
    APIBase() string
    // GitCredential returns the (user, pass) pair for HTTPS git auth:
    // ("x-access-token", tok) on GitHub; ("oauth2", tok) on GitLab.
    GitCredential(tok string) (string, string)
    RepoURL(slug string) string // https://<host>/<slug>.git
}

// SecretWriter is the CI secret plane. Implemented by all four.
type SecretWriter interface {
    SetRepoSecret(name, value string) error
    SetEnvSecret(env, name, value string) error   // GitLab: env-scoped variable
    DeleteEnvSecret(env, name string) error
    SetVariable(name, value string) error
}

// OIDCProvider yields the whole OpenBao jwt-role body, not a hostname —
// bound_claims is forge-shaped ("repository" vs "project_path").
type OIDCProvider interface {
    DiscoveryURL() string
    Issuer() string
    Audience(owner string) string
    BoundClaims(slug string) map[string]string
    MintCIToken(aud string) (string, error) // Actions env vars | GitLab id_tokens
}

// TokenMinter mints a short-lived service credential. All four implement it —
// GitHub via App JWT -> installation token, GitLab via access-token create.
type TokenMinter interface {
    MintEphemeral(scopes []string, ttl time.Duration) (Token, error)
}

// TokenRotator is implemented by GitLab ONLY. The GitHub family cannot rotate
// its root credential via API (App private keys are UI-only), and must not
// pretend to. Callers probe:
//     if r, ok := f.(forge.TokenRotator); ok { ... } else { /* alert-and-escrow */ }
type TokenRotator interface {
    RotateSelf(ttl time.Duration) (Token, error)
    Rotate(tokenID string, ttl time.Duration) (Token, error)
}

// ExpiryProber is implemented by all four, by different means: GitHub reads the
// GitHub-Authentication-Token-Expiration response header; GitLab reads token-info.
type ExpiryProber interface {
    TokenExpiry(tok string) (time.Time, error)
}
```

`Dispatch` is deliberately absent. Triggering a pipeline is a CI-system concern, not
a forge-API concern, and the two are only accidentally the same thing on GitHub. It
belongs with the orchestration layer below.

### Forge selection and config

`spec.instance.forge` becomes real and gains a sibling host:

```yaml
spec:
  instance:
    forge: github | github-enterprise | github-enterprise-server | gitlab
    forgeHost: ghes.corp.example  # required iff forge is -server or gitlab
    repo: org/instance-repo
```

Note this adds a fourth flavor: today's `ForgeGitHubEnterprise` conflates GHEC and
GHES, which the matrix shows are **not** the same forge — GHEC is github.com with a
different tenant, GHES has its own API base *and* its own OIDC issuer. The constant
has to split.

`forge.New(spec)` is constructed once at the `llz` entrypoint and threaded down. The
existing `ghSetSecretFn` / `ghSetRepoSecretFn` / `ghPATProbe` function pointers are
**test seams, not polymorphism** — they get replaced by the interface, not wrapped
around it. `$GITHUB_API` and `$GH_HOST` survive only as env overrides that feed
`forge.New`, and `gh_secrets_native.go`'s `ghAPIBase` is deleted.

### The CI orchestration layer — the honest cost

**The workflows are not thin wrappers, and this is the largest single line item in
the plan.** Measured: ~7,400 lines of Actions YAML across 28 files. Of that, **~80
lines invoke `llz`** — against ~630 lines of inline shell and 53 `run:` steps in
`llz-terraform.yml` alone. The `llz ci` delegation is real but partial. The *job
graph* — matrix strategies, `environment:` gating, `needs:` edges, dispatch-input
fan-out, artifact passing — lives in YAML, not in Go, and does not port mechanically.

The split is uneven, and that is the opening:

| Workflow class | Files | Character | Ports how |
|---|---|---|---|
| Thin callers (`terraform.yml`, `promote.yml`, `cluster-health.yml`, …) | 7 | 0 shell, 0 `llz` — pure `uses:` + input plumbing | mechanically |
| Fat bodies (`llz-terraform.yml` ~1,465L, `llz-bootstrap-openbao.yml` ~1,118L, …) | 7 | the real graph + all the shell | **rewrite** |

**A GitLab CI port is gated on assimilating the fat bodies into `llz ci` verbs
first.** That is not new work invented by this design — it is
[instance-slimming.md](instance-slimming.md)'s **Lever 2**, already staged. Forge
support makes it load-bearing rather than nice-to-have: every line of logic left in
Actions YAML is a line that must be written twice and drift once. The target shape is
that `.gitlab-ci.yml` and the Actions bodies are both thin callers of the same verbs,
and the only per-CI YAML is the job graph.

This is the phase most likely to be underestimated. Budget it as a rewrite.

## Token management and rotation

The OpenBao side does not change. `secret/…` paths, ESO ExternalSecrets, k8s-auth
roles, and the 15m TTLs are forge-independent. What changes is only who mints and who
rotates.

| Credential | GitHub family | GitLab |
|---|---|---|
| CI → OpenBao | Actions OIDC, 15m, keyless | ID token, job-scoped, keyless |
| Instance repo write (values push) | fine-grained PAT — **manual, expiry cliff** | project access token — **self-rotating** |
| CI secret writeback | App installation token (1h) | project access token, self-rotating |
| In-cluster → forge (`harbor-robot-provisioner`, `broad-pat-rotator`) | App private key in OpenBao → install token | access token with `self_rotate` |
| Root secret | **App private key — permanent, UI-only rotation, escrow** | **none** |

**GitHub family.** The end state is the GitHub App: private key in OpenBao (via the
GitHub secrets engine, so the key is configured once and every consumer asks OpenBao
for a scoped ephemeral token rather than each hardcoding an endpoint), minting 1-hour
installation tokens. `actions/create-github-app-token` covers the Actions side and
takes a `github-api-url` input, so it works on GHES unchanged. The permanent private
key is the irreducible floor; it is escrowed like `OPENBAO_SEAL_KEY` already is, and
rotated by a human on our schedule rather than GitHub's.

**GitLab.** No App concept, and none needed. A project access token with
`self_rotate` renews itself; the in-cluster rotator calls `RotateSelf` on the same
~80-day cadence the Linode credential rotator already uses, writes the new value to
OpenBao, and drains. There is no root key to escrow and no expiry cliff to alert on.
**The GitLab path is the one that fully solves the problem this design exists to
address** — which is a good reason not to let the interface drag it down to the
GitHub floor.

**What stays manual, on every forge.** `OPENBAO_SEAL_KEY`, the recovery keys, and
`TF_STATE_*` cannot live in OpenBao — they are what unseal it, or they predate the
cluster. They are written to the CI secret plane at bootstrap and stay there. That is
a chicken-and-egg floor, not a forge property.

**Verify before drain, on both.** Whatever mints, the rotator must probe the new
credential before draining the old one. The existing objkey path in
`ci_rotate_linode_creds.go` does **not** — it mints, writes OpenBao, and drains with
no probe, while the PAT path calls `Verify` first. Do not carry that asymmetry into a
second forge; fix it in Phase 2 or inherit it four times.

## Wizard changes

Forge selection becomes the **first** question, because it determines every question
after it — how many tokens, what they are called, what URL mints them, and whether
rotation is a thing the operator has to think about at all.

- **Forge + host prompt.** `forge` drives `forgeHost` (required for `-server`/gitlab,
  suppressed otherwise) and validates reachability before anything else runs.
- **Per-forge minting URLs.** The `ghTokenURL` / `ghFineGrained*URL` helpers become
  forge methods. This also **fixes the live GHE bug** where `ghHost()` is resolved and
  then ignored. GHES: `https://HOST/settings/personal-access-tokens/new`. GitLab:
  `https://HOST/<group>/<project>/-/settings/access_tokens`.
- **Per-forge scope vocabulary.** GitHub fine-grained "Contents: write / Actions:
  write / Secrets: write" has no GitLab equivalent; GitLab wants `api`,
  `write_repository`, `self_rotate`. The wizard must speak each forge's language, not
  translate GitHub's into it.
- **Different token counts.** A GitLab operator mints **fewer** tokens and is told
  they self-rotate. A GHES operator gets the App flow. The wizard should not present a
  GitHub-shaped checklist to a GitLab adopter.
- **Per-forge validation probes.** `token_validate.go`'s hardcoded
  `"api.github.com unreachable"` string becomes forge-derived.

Fine-grained PATs on GHES are worth an explicit wizard note: they are GA as of recent
versions, but the docs still carry *"cannot accomplish every task that a personal
access token (classic) can"* — the stated gaps include **no access to `internal`
repositories** and **no multi-org access**. Both land on us. An enterprise adopter
using `internal` visibility cannot use a fine-grained PAT, which is an argument for
routing GHES adopters to the App flow rather than the PAT flow by default.

While here: `docs/secrets.md`, `docs/quickstart.md`, and
`runbooks/bootstrap-openbao.md` all say `OPENBAO_SECRETS_WRITE_TOKEN` must be a
**classic** PAT, while the workflows and the wizard both use fine-grained and the
expiry audit actively *fails* never-expiring classic PATs. Nothing needs a
classic-only scope; the real constraint is Environment admin on every `infra-<env>`,
which is an org-role property, not a token type. Fix the docs in Phase 0 — a forge
design that inherits a false premise about token types starts wrong.

## Bootstrap / cold-start

Unchanged in shape, and the ordering constraint is forge-independent: the CI secret
plane must hold `TF_STATE_*` and the forge credential before any cluster exists;
OpenBao's seal/recovery material is written back to the CI secret plane at bootstrap
because it cannot live in the thing it unseals.

The one new cold-start edge is GitLab-specific and benign: `self_rotate` cannot
bootstrap itself. The operator mints the first project access token by hand, exactly
once; every rotation after that is the token renewing itself. This is strictly better
than the GitHub family, where the human-minted artifact (the App private key) is
permanent rather than a seed.

## Failure modes

| Failure | Behavior |
|---|---|
| `forge:` set, `forgeHost:` absent on `-server`/gitlab | `clusterspec` validation error at render — never reaches a cluster |
| Forge host unreachable at wizard time | fail fast, before any token is minted |
| GitLab `RotateSelf` fails | old token untouched and still valid; next tick retries; alert on approaching expiry |
| GitHub App private key lost | **unrecoverable without escrow** — same class as `OPENBAO_SEAL_KEY`; escrow it identically |
| GitHub App key compromised | mint a second key, roll consumers, delete the first — zero downtime (25-key ceiling) |
| Mint succeeds, new credential is bad | **the drain must not proceed** — probe first (see §Token management) |
| `forge` value valid but unimplemented | must be a hard error, not silence. **This is today's behavior and the bug that motivates Phase 0.** |
| Template repo goes private | repo-server's anonymous clone breaks fleet-wide; `kustomize.go` has no credential path |

## Observability

`llz_token_expiry_timestamp_seconds` and the `token-inventory` ConfigMap already
exist and are the natural home. Two changes:

- **Label by forge.** The same metric, with a `forge=` label, so a mixed fleet is one
  dashboard rather than four.
- **Emit `llz_forge_rotation_supported`** (or fold it into the inventory as a
  capability field). A GitLab instance reporting "manual rotation pending" is a bug; a
  GitHub instance reporting it is Tuesday. The alert rule needs to tell them apart,
  and today's `LLZTokenExpiringSoon` cannot.

Also fold in the coverage gap the inventory has today: `ci_token_inventory.go` probes
only `OPENBAO_SECRETS_WRITE_TOKEN` and `APL_VALUES_REPO_TOKEN`. `E2E_DISPATCH_TOKEN`
has an expiry and **no** lead-time alerting — it is only caught by a fail-fast probe
at run time. Whatever the forge, the inventory should enumerate from the forge, not
from a hand-maintained list.

## What this retires (once implemented + e2e-validated)

- The false docstrings on `validate.Forge`'s const block and `clusterspec.Instance` —
  either the claims become true or the constants go. **Phase 0, cheap, do it first.**
- `gh_secrets_native.go`'s `ghAPIBase` test seam, replaced by `forge.APIBase()`.
- The `$GITHUB_API` / `$GH_HOST` / `ghAPIBase` triple, collapsed to one config axis.
- `ForgeGitHubEnterprise` as a single flavor — splits into GHEC and GHES.
- ADR 0003's dangling reference to unmerged PR #16.
- Ultimately: `OPENBAO_SECRETS_WRITE_TOKEN` and `APL_VALUES_REPO_TOKEN` as
  human-owned PATs on the GitHub family (→ App), and entirely on GitLab (→
  self-rotating).

Nothing is retired until its replacement passes a green `release-e2e` on a real
cluster.

## Rollout (phased — cheapest and most-honest first; each phase independently shippable)

1. **Phase 0 — stop lying.** Make `spec.instance.forge` reject unimplemented values
   with a real error; fix the docstrings; fix the classic-vs-fine-grained docs. No
   abstraction, no behavior change. Hours, not days, and it removes the trap an
   adopter hits first.
2. **Phase 1 — one host axis.** Collapse `$GITHUB_API` / `ghAPIBase` / `$GH_HOST` into
   one resolved config; fix `gh_secrets_native.go` to honor it; fix the wizard's
   minting URLs to honor `ghHost()`. **GHEC works after this**, near-free, and both
   live bugs from §Problem are closed. Still no interface.
3. **Phase 2 — `internal/forge` + GitHub implementation.** Pure refactor behind the
   interfaces; existing GitHub e2e must stay green with zero behavior change. Fix the
   objkey verify-before-drain gap here, before it is inherited four times.
4. **Phase 3 — GHES.** Split the flavor; OIDC issuer/discovery/audience become
   forge-derived; the OpenBao jwt-role body becomes forge-shaped; NetworkPolicy egress
   becomes host-derived. **Gated on access to a real GHES instance** (§Open questions).
5. **Phase 4 — GitHub App + OpenBao GitHub secrets engine.** The token work above.
   Independent of GitLab; delivers the expiry-cliff fix to the three GitHub forges.
6. **Phase 5 — assimilate the fat workflow bodies into `llz ci` verbs.**
   [instance-slimming.md](instance-slimming.md)'s Lever 2, promoted to a hard
   prerequisite. **This is the long pole.** GitLab cannot start until it is done, and
   it is worth doing even if GitLab is cancelled.
7. **Phase 6 — GitLab.** `.gitlab-ci.yml` as a thin caller; GitLab `SecretWriter`
   (smaller than GitHub's — no sealed box); ID-token OIDC with `project_path` claims;
   `TokenRotator` with `self_rotate`. **Gated on the e2e harness having a GitLab
   project**, which today it does not.

Phases 0–2 are worth doing on their own merits even if no adopter ever asks for a
second forge: they close two live bugs, delete a lying field, and fix a rotation gap.
That is the argument for starting now rather than when a customer forces it.

## Open questions

- **Where does a GHES instance come from for e2e?** Phases 3 and 6 are unvalidatable
  without a real GHES appliance and a real GitLab project in the harness. Today
  `release-e2e` is GitHub.com-only. **This is the question that most determines
  whether this plan is real**, because the repo's own governing discipline is that a
  green e2e on a real cluster is the acceptance test — and a forge implementation that
  has never talked to its forge is not a forge implementation. Both outcomes are
  workable: with a harness, phases proceed as written; without one, they ship as
  explicitly-unsupported and this doc should say so rather than implying coverage.
- **Does the template repo stay public, permanently?** §The two-forge split rests on
  it. `kustomize.go`'s anonymous clone has no credential path, and adding one is a
  fleet-wide change to the repo-server's auth. Worth an explicit decision now rather
  than discovering it during a private-repo migration.
- **Does `infra-<env>` survive on GitLab?** Environment-scoped variables are close but
  not isomorphic, and the concept is load-bearing across seven workflows plus
  `tokens.go` and `regenroot.go`. Either GitLab's environment scoping is close enough,
  or the `infra-<env>` pattern itself needs a forge-neutral reformulation. **Verify
  against a real GitLab project before designing around it** — the design works either
  way; the answer only decides whether `SetEnvSecret` is one method or two.
- **Does the OpenBao GitHub secrets engine carry its weight?**
  `vault-plugin-secrets-github` is third-party and not in OpenBao's first-party plugin
  collection — we would own the build and upgrade path. The alternative is minting
  installation tokens in `llz` directly from a key in KV. The design works either way;
  the answer only decides where the JWT-signing code lives.
- **Is GHEC data residency (`ghe.com` tenants) in scope?** It moves the API base but
  nothing else. Probably falls out of Phase 1 for free — confirm rather than assume.

## See also

- [../secrets.md](../secrets.md) — the credential inventory this design re-homes.
  Note it currently contradicts the code in several places (TF-state rotation is
  scheduled though the doc says it is not; `linodeCredRotator` no longer exists as a
  component; the classic-PAT claim above). Worth reconciling alongside Phase 0.
- [credential-single-pane-incluster.md](credential-single-pane-incluster.md) — the
  in-cluster rotation model the GitLab `TokenRotator` slots into.
- [instance-slimming.md](instance-slimming.md) — Lever 2 is Phase 5 here.
- [../adr/0003-vendor-actions-and-bodies-into-instances.md](../adr/0003-vendor-actions-and-bodies-into-instances.md)
  — already did the portability work for the workflow layer and names GHE/GitLab as
  the motivation. **Build on it; do not relitigate it.**
