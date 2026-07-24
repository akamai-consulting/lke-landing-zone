---
name: onboard-adopter
description: Guide a new system team from nothing to a converging LKE-Enterprise + apl-core cluster using the llz CLI - accounts, install, scaffold, credentials, build, convergence, and the post-bootstrap manual steps. Use when someone asks how to stand up an instance, onboard to the landing zone, "get started", or is stuck partway through the quickstart flow.
---

# Onboard a new adopter

`docs/quickstart.md` is the canonical fast path and `docs/adopter-guide.md` the
full rationale — read the quickstart in full before guiding anyone; this skill
is the coaching layer on top, not a replacement.

## The flow (each step's authority is the quickstart section it cites)

1. **Accounts first** (§1 — longest lead time is the Linode account):
   Linode with **LKE-Enterprise** (`+lke` versions, not standard LKE), Akamai
   App Platform (apl-core) entitlement, a GitHub org + instance repo. InfoSec
   path: `docs/infosec/linode-account-request-checklist.md`.
2. **`gh` auth BEFORE anything** (§2): `gh auth status --hostname github.com ||
   gh auth login --hostname github.com`. The installer and every GitHub-touching
   `llz` command drive `gh`.
3. **Install `llz`** (§2): the one-liner installer from the template repo
   (checksum-verified, installs to `~/.local/bin`). Keep current with
   `llz self-update`.
4. **Scaffold** (§3): `llz new <instance> --push --yes`, then
   `llz env add <env> --region <r> --obj-cluster <c>` (both flags required;
   list OBJ clusters with `linode-cli object-storage clusters-list`). The spec
   (`landingzone.yaml` + `environments/<env>.yaml`) is the source of truth —
   edits go through `llz env set` / `llz spec set` / `llz env edit`, never
   hand-edits of rendered tfvars (they are gitignored build artifacts).
5. **Readiness**: `llz doctor --env <env>` is the single "am I ready to build?"
   gate — run it after every fix until green.
6. **Build** (§4): `llz up <env> --yes` chains tokens → doctor → build and
   stops at the first failure.
7. **Post-bootstrap manual steps** — the two things the bootstrap cannot do
   (runbook: `docs/runbooks/bootstrap-openbao.md`): copy the static seal key +
   recovery keys 4 & 5 + root token to offline storage (shown ONCE), and delete
   `OPENBAO_ROOT_TOKEN` from the `infra-<env>` environment if seeded
   (`llz status` nags until done).
8. **Finish + verify**: `llz bootstrap dns <env> --yes` (needs
   `LINODE_DNS_TOKEN`), then `llz status <env>` until converged.

## How the agent should behave in this flow

- **Run freely (read-only)**: `llz doctor`, `llz status`, `llz env list/show`,
  `llz components`, `llz render --diff`, `llz drift`, and any `--dry-run`.
- **Never run for the user**: `llz up` / `llz tokens` — they are INTERACTIVE
  (browser links, pasted PATs) and must be run by a human at a terminal.
  `--yes` authorizes cloud mutation; it does not make the run unattended.
  Coach through the output instead.
- Diagnose "not ready" from `llz doctor` output — it names each missing item
  and the command that fixes it. Don't guess ahead of it.
- Second/HA deployment: only after the first has FULLY bootstrapped (Harbor
  robot credential ordering — see the bootstrap runbook). HA pairs need a
  shared `--ha-group`, opposite `--ha-role`, and distinct `--subnet-cidr`s.
- Day-2 upgrades: `llz self-update` then `llz upgrade` moves the scaffold +
  first-party pins; Renovate PRs move charts + external actions. Never suggest
  hand-bumping first-party pins.
