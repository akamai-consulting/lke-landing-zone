# ADR 0007 — Encrypt Terraform state and plans at rest

Status: **accepted**, phase 1 shipped. Phase 2 (enforcement) is a follow-up.

## Context

Terraform state contains every provider-computed attribute in plaintext. The
`sensitive = true` annotation controls CLI **display** only — it has no effect on
what is written to the state file. So the landing zone's state bucket holds, in
the clear:

| Root | Secret material in state |
|------|--------------------------|
| `cluster` | `kubeconfig_raw` — a **cluster-admin** credential |
| `databases` | `root_password` — the `akmadmin` password for every Managed Postgres cluster |
| `object-storage` | Object Storage key material |

Anyone holding `TF_STATE_ACCESS_KEY` can read all of it. The bucket has no
server-side encryption configured (the S3 backend blocks set `skip_*` flags but
never `encrypt`), and server-side encryption would in any case defend against
disk theft rather than against the access key, which is the real blast radius.

> **Measured 2026-07-31** (this paragraph previously said Linode's honouring of
> `x-amz-server-side-encryption` was *unverified*; it has now been probed against
> a scratch bucket on `us-ord-10`, since deleted):
>
> | Request | Result |
> |---|---|
> | plain `PUT` then `HEAD` | `200`, **no** `x-amz-server-side-encryption` — nothing applied by default |
> | `x-amz-server-side-encryption: AES256` (SSE-S3) | **`400 InvalidArgument`** |
> | SSE-C (customer-provided key) | `200`; `HEAD` without the key `400` — works |
> | `PutBucketEncryption` / `GetBucketEncryption` | **`501 NotImplemented`** |
>
> So SSE-S3 is not merely unverified, it is **rejected**, and there is no
> bucket-level default either. This matters beyond the state bucket: Harbor's
> registry (`encrypt: true`) and Loki (`sse.type: SSE-S3`) can each request SSE-S3
> in one line of values, and on Linode that would return 400 on every blob push
> and every chunk flush rather than degrading to plaintext. SSE-C is the only mode
> Linode implements and no writer here can emit it. See the
> `linode_object_storage_bucket` entries in `atRestAllowed`
> (`tools/internal/extensions/lifecycle/atrest/atrest.go`), which carry the same numbers and the
> conditions that would retire them.

The question that prompted this was narrower: *can we keep the Managed Postgres
admin password out of state?* The answer is **no** — `root_password` is a
provider-**Computed** attribute, so it is persisted unconditionally. There is no
input form of it, so OpenTofu 1.11's write-only (`_wo`) arguments do not apply,
and dropping the `connections` output removes only the second copy (the state's
root `outputs` block), not the attribute on the resource.

Per-field mitigation is therefore not available, and would be the wrong altitude
anyway: the DB password is not even the most sensitive thing in the bucket.

## Decision

Enable **OpenTofu native state and plan encryption** (available since 1.7) on all
four roots — `cluster`, `databases`, `object-storage`,
`vpc`.

### Key provider: `pbkdf2`, passphrase from a GitHub secret

`key_provider "openbao"` (transit) was considered and rejected: OpenBao runs
**inside the cluster Terraform provisions**, so the `cluster` root's state
encryption would depend on the thing that root creates. Circular, and it would
make state unreadable exactly when OpenBao is down — which is when the state is
most needed. There is no Linode KMS.

`pbkdf2` with a passphrase from `TF_STATE_ENCRYPTION_PASSPHRASE` has no such
dependency.

### Split: posture in code, key material in the environment

Each root carries an `encryption.tf` with **only** the `state`/`plan` blocks. The
key provider and method arrive via the `TF_ENCRYPTION` environment variable,
which `.github/actions/terraform-init` builds from the secret. OpenTofu merges
the two.

The split is the point. With the whole configuration in the env var, a hand-run
`tofu apply` without `TF_ENCRYPTION` would **silently write plaintext state**.
With the block present in code, that run fails instead.

Because it fails with OpenTofu's own unhelpful message — *"Invalid expression …
A single static variable reference is required"*, pointing at the encryption
block rather than the missing secret — `terraform-init` preflights the secret and
fails with an explicit one. It also rejects a passphrase outside `[A-Za-z0-9+/=_-]`:
the value is interpolated into an HCL string, so a quote or backslash could close
the string and **inject encryption configuration** (e.g. substitute
`method.unencrypted`).

### The split's cost, and how it is paid

The same property that makes the tripwire work makes it fire on the OPERATOR.
Runbooks legitimately ask for a hand-run `tofu` — `apply -refresh-only` after an
lke-admin rotation, `state pull` during a key rollover — and every one of them
landed on that unhelpful message. The documented remedy was to hand-assemble a
twelve-line HCL document with a live passphrase in it, once per terminal, which
put the secret in shell history and made the sharpest edge in the local flow a
copy-and-paste ritual.

The material was never actually missing: `llz tokens` writes
`TF_STATE_ENCRYPTION_PASSPHRASE` to `.llz/secrets.env` (mode `0600`, gitignored)
at onboarding. Nothing connected the cache to an invocation. `llz tofu` does:

- **`llz tofu -- <args>`** builds `TF_ENCRYPTION` from the cache, adds the
  object-storage backend credentials and the Linode token, and execs OpenTofu.
- **`llz tofu --shell-init`** prints an rc snippet defining a `tofu` function that
  routes through the above; the dev container installs it on create. This is the
  shape to prefer: the credentials reach the CHILD process only and never enter
  the interactive shell.
- **`llz tofu --export`** hands the same environment to a shell, for a script
  running several commands against one environment. The values go to a private
  `0600` file and stdout carries only `. <path>; rm -f <path>` — so nothing a
  recorder sees (scrollback, `script(1)`, a CI log, `set -x`) contains the
  passphrase. It refuses a terminal, because with no `eval` around it the handoff
  would leave an unconsumed passphrase on disk; every `llz tofu` invocation also
  sweeps files older than a minute, so a mistyped `eval` is cleaned up by the next
  use rather than by the operator remembering.

  Printing the exports to stdout was the original design, and it disclosed a live
  instance's passphrase into a session transcript during this feature's own
  development — a pipe is the *intended* destination, so the terminal guard was
  never going to fire. The file handoff protects the **transport**; it does not
  change the fact that after the `eval` the value is in the shell's environment,
  which is the reason `--shell-init` is the recommendation. A FIFO would keep the
  bytes off disk entirely and does not work here: `eval "$(…)"` drains stdout to
  completion first, so `llz` has exited before the shell sources anything.
- **Every Terraform shell-out `llz` makes** resolves the same way, at the
  `internal/shared/tfbin` chokepoint — so `llz ci tf-output` and
  `llz ci fetch-kubeconfig-state` work in a local checkout too.

**Hydration never REPLACES.** A variable already present in the process
environment is left alone, and outside an instance checkout nothing is
contributed at all.

It is worth being precise about what that does *not* say, because the imprecise
version ("it can only add, so the worst case is that it changes nothing") was
written here first and is wrong. Introducing a variable that was absent is a real
change, and the mapping makes it reachable: the cache stores `LINODE_API_TOKEN`
while the provider reads `LINODE_TOKEN`, so a process holding the former and
deliberately not the latter would acquire a cloud credential it did not have.
The absence of a credential can itself be a control.

In CI it is nonetheless a no-op, but by two checkable facts rather than by
construction: there is no `.llz` cache on a runner, and the workflow steps that
export `LINODE_API_TOKEN` (`drain-obj-buckets`, `temp-objkey create`, the
credential probes) shell out to no Terraform command. Both are worth re-checking
when a workflow gains a Terraform step.

**A child process cannot export into its parent shell**, which is why there are
two shapes rather than one command that "just fixes the environment". Nothing
`llz` runs can make a LATER bare `tofu`, in an already-running terminal, see the
variable.

### One document, two emitters

The HCL is now emitted by `internal/shared/tfenc`, and the composite action still
emits it in shell for CI. They must agree byte for byte — state written under one
is unreadable by the other — so `tfenc_test.go` executes the **shipped action's
real shell body** and diffs the result, for the happy path, the rotation window,
and every rejected input. A separate gate runs a real `tofu init` against the
**shipped `encryption.tf`**, asserting both arms: it fails without the document
and succeeds with it.

### Two-phase rollout, because `enforced` and `fallback` are exclusive

OpenTofu rejects the combination outright — *"Unable to use unencrypted method
since the enforced flag is set"* — so an instance with existing plaintext state
cannot go straight to enforcement.

- **Phase 1 (shipped).** `fallback { method = method.unencrypted.migrate }`,
  `enforced` **not** set. Reads existing plaintext state, writes encrypted. A
  plain no-op `tofu apply` performs the migration.
- **Phase 2 (follow-up).** Once every deployment's state is migrated, delete the
  `fallback` block and set `enforced = true`. An unencrypted write is then
  refused rather than merely unusual.

Skipping to phase 2 on a live instance breaks it: the fallback is what makes
pre-encryption state readable.

## Verified

Against OpenTofu 1.12.3 locally, not inferred from documentation (CI runs 1.12.5
in `ci-tofu` — see [ADR 0008](0008-opentofu-migration.md), which had to land
first: this `encryption` block does not exist in HashiCorp Terraform, and CI ran
Terraform 1.9.8 until that migration):

1. A code-side `encryption` block merges with `TF_ENCRYPTION`, producing state
   with `encrypted_data` and no plaintext resource attributes.
2. Without `TF_ENCRYPTION`, the run **fails** — it does not fall back to
   plaintext.
3. `enforced = true` together with an `unencrypted` fallback is a hard error.
4. Phase 1 migrates plaintext → encrypted on a no-op apply; phase 2 then reads it.
5. A **wrong** passphrase is a hard decryption failure, not a silent fallback.

## Consequences

- **Key escrow is now load-bearing.** Losing `TF_STATE_ENCRYPTION_PASSPHRASE`
  makes every state file unrecoverable — the same class of risk as
  `OPENBAO_SEAL_KEY`, and it needs the same offline-escrow discipline. This is a
  real new failure mode traded for confidentiality, and it should be recorded as
  such rather than presented as free.
- **At rest only.** CI decrypts in order to work, so this is not a defence
  against a compromised runner. It defends the state bucket, its backups, and
  anyone who obtains the object-storage key.
- **A new required secret** on every instance. `terraform-init` fails loudly
  without it, so the failure is legible, but adopters must add it before their
  next Terraform run.
- `terraform-modules/` are unaffected — modules have no state of their own.

## Alternatives rejected

| Option | Why not |
|--------|---------|
| Drop the `connections` / `root_password` outputs | Removes one of two copies; the resource attribute remains. Also breaks `seed-db-admin`. |
| Backend SSE (`encrypt = true`) | **Rejected by Linode OBJ** — measured `400 InvalidArgument` (see Context). Would also have defended against disk theft rather than against the access key. |
| `key_provider "openbao"` | Circular — OpenBao runs inside the cluster the `cluster` root provisions. |
| Write-only (`_wo`) attributes | Cover values *you supply*; `root_password` is provider-computed. |
| `terraform state rm` | Abandons management of the resource. |

## Related

- The **PAT-rotation-locus** question — same "where does the credential live" framing. (Reserved as ADR 0001; not yet written — see [the ADR index](README.md).)
- [docs/designs/shared-managed-postgres.md](../designs/shared-managed-postgres.md)
  — the rotate-on-create half, which limits how long the state copy is *live*
  rather than how readable it is. The two are complementary: encryption protects
  the file, rotate-on-create ensures the credential in it was never the only copy.
