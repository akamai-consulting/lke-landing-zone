# `llz-validate-credentials` — maintainer rationale

`instance-template/.github/workflows/llz-validate-credentials.yml` probes every
pipeline credential an instance holds — validity **and scope** — against the
values CI is actually handed, on a daily cron and on demand. It is **vendored
verbatim into every customer instance by copier**, so it runs self-contained.
See `docs/adr/0003-vendor-actions-and-bodies-into-instances.md`.

Because the YAML is copied into instances where it can never be updated in
place, long-form maintainer archaeology lives here in the template repo instead
of in the workflow body. This document is the archive; the inline comments are
the 3am debugging aids.

---

## The incident this exists for

`akamai/gsap-apl`, run `33556210825` (prod promote, 2026-09-01). The Shared VPC
job failed 35 seconds in:

```
OPENBAO_SECRETS_WRITE_TOKEN    ⚠ valid, expires in 9d — rotate soon
  └ scope   ✓ authorized to write infra-<region> environment secrets
  └ scope   ✗ DENIED — authenticates, but is NOT authorized to read/write
            REPO-level Actions secrets (HTTP 403) — under-scoped, not expired
```

The operator had run `llz doctor --env prod` first. It reported
`OPENBAO_SECRETS_WRITE_TOKEN … ✓ valid ✓ scoped` and
`✓ ready — every required value is set, and every probeable token is valid and
scoped for its job.`

Both were telling the truth about **different tokens**. Reconstructed from the
GitHub API's `updated_at` and the local file's mtime:

| time (UTC) | event |
|---|---|
| 20:29:23 | six secrets written to `infra-prod` — a bulk `llz secrets push` |
| 20:31:05 | `.llz/secrets.env` modified — the re-scoped replacement PAT pasted in |
| 20:36:50 | the promote reads `infra-prod`: the **pre-fix** token, 9d, 403 |

The local copy expired in 89d and passed both scope probes; the pushed copy
expired in 9d and failed one. `doctor` probed the former, CI got the latter.

## Why `doctor` cannot close this on its own

GitHub Actions secrets are **write-only** — nothing, including `llz`, can read a
stored value back. So `doctor`'s `VALID`/`PERMS` columns necessarily describe
the copy in `.llz/secrets.env`, while the `set` column describes GitHub. It goes
green whenever your laptop is correct, however stale the pushed copy is. That is
structural, not a bug, and it means **a green `doctor` is not evidence about
what CI will receive**. Only a job running in the environment is.

`llz tokens --env <env>` does not close it either: it is an idempotent
*provisioner* that fills in what is MISSING ("Skips anything already
configured") and never compares values, so with every secret already present it
is a no-op. Its own output says so, which is the right place for that warning —
it just cannot detect that it applies to you.

## Why not fold it into `llz-scheduled-checks`

That workflow already runs daily and already fans out over the same discover
shim, so the job looks like it belongs there. It does not:

- **A dispatch has to be cheap and cluster-free.** `scheduled-checks`'
  `workflow_dispatch` runs the weekly cluster checks, the drift report and the
  credential single pane, most of which need a kubeconfig and the shared LKE-E
  control-plane ACL. Answering "will my pushed secrets work?" must not cost that,
  and must still answer on a half-built or wedged instance where no cluster
  exists to fetch a kubeconfig from.
- **The failure modes are different.** The credential single pane
  (`llz ci token-inventory`) measures **expiry** and publishes metrics; it is
  gated on `steps.kubeconfig.outputs.available == 'true'`. It would not have
  caught the incident above — the token's problem was scope, and scope is
  exactly what expiry cannot see.

## Rejected: a `deployment` input to probe one environment

The obvious ergonomic knob, dropped for two reasons. Matrix jobs run in
parallel, so probing every deployment costs the same wall-clock as probing one —
there is nothing to optimise. And a free-form string input naming a deployment
that does not exist would land in `environment: infra-<typo>`, which GitHub
resolves by **implicitly creating** that environment: a dispatch typo would
silently litter the repo with empty, unprotected environments. Validating the
input against the discover output would fix that, at the cost of a whole extra
job to buy back a saving of zero.

## `GH_REPO` and `REGION` are both load-bearing

The scope probes SKIP when they cannot build a request, and a skipped probe
still prints a passing-looking line. Measured against a real instance:

| env | result |
|---|---|
| neither | `– repo/region unknown` **and** `– repo unknown` — validity only |
| `GH_REPO` only | repo-level probe runs; environment probe still `repo/region unknown` |
| both | both probes run |

So the partial case is the dangerous one: `GH_REPO` alone looks like it works —
one scope line goes green — while the environment-secret half stays dark. This
is the same hazard `llz-terraform.yml` calls out at its own pre-flight, and the
reason both variables are set together with a comment tying them to this note.

Note the asymmetry with `GITHUB_REPOSITORY`: the probes read `GH_REPO`, and
setting the more familiar `GITHUB_REPOSITORY` instead accomplishes nothing.

## What the local side does instead

A metadata comparison needs no CI run and catches the incident above outright:
`llz tokens` and `llz doctor` now date each pushed secret's `updated_at` against
the mtime of `.llz/secrets.env` and say which of the two states they found. See
`tools/internal/shared/envreq/drift.go`.

That check reports **both** directions, and the second is the one a naive version
gets backwards. `llz ci rotate-broad-pat` publishes a fresh `LINODE_API_TOKEN` to
every `infra-<deployment>` and revokes the one it replaced; `llz-secret-rotation.yml`
does the same for the `TF_STATE_*` pair. For those three, a pushed copy *newer*
than the local file means the local copy is the stale, already-revoked one — so
re-pushing it would overwrite a live credential with a dead one. That is also why
`llz tokens` does not simply auto-sync, and why the push it offers carries only
the secrets that are behind.

Where llz pushed the value itself, the comparison is **exact** rather than a
timestamp guess: `llz secrets push` records a SHA-256 of each value it sends
(`.llz/.push-state.json`, digests only, in an already-gitignored directory), so a
later run can tell a changed local value from an unchanged one, and llz's own
push from somebody else's write.

That precision was not optional. The first version ordered file mtimes alone, and
on its first run against a real instance it reported five secrets the operator had
just pushed **by hand** as "CI rotated these, your copy is stale" — pointing at the
opposite of the truth, in the one direction where acting on the advice pushes a
revoked credential into CI. A push llz performed looks exactly like a rotation
from the outside; only a record of that push separates them.

The mtime path survives as the fallback for credentials with no such record — an
instance provisioned before the log existed, or a secret set by hand in the GitHub
UI. There the note says only what ordering supports, and names both possibilities
rather than picking one.

This workflow is what covers the rest — the class no local check can see at all:
pushed from another machine, revoked after the push, scope narrowed later. The
remaining piece is a remote mode for `llz doctor` that dispatches this workflow,
polls it, and merges the verdict into its `VALID`/`PERMS` columns so the table
describes the pipeline rather than the laptop. Deliberately not in this PR: it
needs a dispatch-and-wait UX decision, and the workflow is independently useful
on its cron.

## Environment protection

The probe job takes `environment: infra-<deployment>`, so on an instance whose
`infra-*` environments carry **required reviewers** every run — including the
daily cron — will queue for approval rather than failing. Instances that lock
those environments to a branch policy only (the `llz` default) are unaffected.
Anyone adding reviewer protection should expect to move this workflow to a
dispatch-only trigger, or accept a daily approval prompt.
