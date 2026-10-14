# ── State + plan encryption at rest (OpenTofu native) ────────────────────────
#
# WHY: Terraform state contains every provider-computed attribute in plaintext,
# including the ones marked `sensitive` — that annotation controls CLI DISPLAY
# only, never what is written. So the state bucket already holds, in the clear:
#
#   - cluster/       kubeconfig_raw — a CLUSTER-ADMIN credential
#   - databases/     root_password  — the akmadmin password for every Managed
#                                     Postgres cluster (a provider-COMPUTED
#                                     attribute: there is no way to keep it out)
#   - object-storage/               the OBJ key material
#
# Anyone holding TF_STATE_ACCESS_KEY can read all of it. That — not any single
# field — is the exposure, and encrypting the state file is the only fix that
# addresses it in one place instead of per-field.
#
# HOW: the key material is NOT here. This file carries only the enforcement
# posture; the key provider and method arrive via the TF_ENCRYPTION environment
# variable, which .github/actions/terraform-init exports from the
# TF_STATE_ENCRYPTION_PASSPHRASE secret. OpenTofu MERGES the two, so the
# passphrase never lands in the repo, in a tfvars file, or in state itself.
#
# Splitting it this way is deliberate: with the whole configuration in the env
# var, a hand-run `tofu apply` without TF_ENCRYPTION would silently write
# PLAINTEXT state. With the block below present in code, the same run fails
# instead. (The failure message is OpenTofu's own and is not obvious —
# "Invalid expression ... A single static variable reference is required" —
# which is why terraform-init preflights the secret and says so plainly.)
#
# RUNNING TOFU BY HAND? Do NOT assemble TF_ENCRYPTION yourself. `llz tofu`
# builds it from the passphrase already cached in .llz/secrets.env, adds the
# object-storage backend credentials, and passes everything after `--` straight
# through — it is the same document CI gets, from the same secret:
#
#   llz tofu --region <env> -- init -upgrade
#   llz --yes tofu -- apply -refresh-only -auto-approve -var-file=<env>.tfvars
#
# For a plain `tofu`, have your shell take the environment instead:
# `eval "$(llz tofu --export)"`, or add `eval "$(llz tofu --shell-init)"` to your
# rc once. The dev container does the latter for you. See
# docs/playbooks/operator-onboarding.md.
#
# ── MIGRATION: this is PHASE 1 of 2 ──────────────────────────────────────────
#
# `enforced` and the `unencrypted` fallback are MUTUALLY EXCLUSIVE — OpenTofu
# rejects the combination outright ("Unable to use unencrypted method since the
# enforced flag is set"), so an existing instance cannot go straight to enforced.
#
#   PHASE 1 (this file): fallback, NOT enforced. Reads existing plaintext state,
#           writes encrypted. A plain no-op `tofu apply` performs the migration —
#           verified locally against OpenTofu 1.12.3.
#   PHASE 2 (follow-up): once every deployment's state has been migrated, delete
#           the `fallback` block and set `enforced = true` in its place. From
#           then on an unencrypted write is refused rather than merely unusual.
#
# Do NOT skip to phase 2 on a live instance: the fallback is what makes the
# pre-encryption state readable, and without it the first plan fails to decrypt.
#
# KEY ESCROW: losing TF_STATE_ENCRYPTION_PASSPHRASE makes the state file
# unrecoverable — a wrong passphrase is a hard decryption failure, not a
# fallback to plaintext (also verified). Escrow it offline with the same
# discipline as OPENBAO_SEAL_KEY. See docs/adr/0007-terraform-state-encryption.md.
#
# SCOPE: this protects state AT REST. CI still decrypts in order to work, so it
# is not a defence against a compromised runner — it is a defence against the
# state bucket, its backups, and anyone who obtains the object-storage key.
terraform {
  encryption {
    state {
      fallback {
        method = method.unencrypted.migrate
      }
    }
    plan {
      fallback {
        method = method.unencrypted.migrate
      }
    }
  }
}
