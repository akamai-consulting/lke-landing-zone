# Runbook: onboard an APL user to write team secrets (no root token)

Give a human operator scoped OpenBao **write** access via their APL/Keycloak
identity, so day-2 secret writes stop needing the root token. Background: the
team-scoped-credentials design (#299 / ADR 0004).

This builds on **native apl-core teams**: `llz render` turns each `spec.teams`
entry into a `teamConfig.<name>` entry, and apl-core provisions the team
namespace **and** a Keycloak realm group + realm role `team-<name>`. We do **not**
create groups or claim mappers ourselves — apl-core already ships a `groups`
claim (a realm-role mapper) on the default `openid` client scope.

There are three actors: the **team** (an apl-core team ↔ an OpenBao policy), the
**platform admin** (declares the team), and the **operator** (logs in and
writes).

## Turnkey path (new clusters): just declare the team

New instances already have a team: `llz new` scaffolds one from the
`openbao_team` question (default `platform`) into `landingzone.yaml`. To add
another team, or on a hand-authored instance, declaring it is **all** the
platform admin does — add it to `landingzone.yaml`:

```yaml
spec:
  teams:
    - name: gsap                  # apl-core team id; names the OpenBao gsap-writer policy + role
      openbaoSubtree: secret/gsap # → write on secret/data/gsap/*
```

`llz render` writes `teamConfig.gsap` into the apl-values overlay, so apl-core
creates the `team-gsap` group/role on convergence. The `Bootstrap OpenBao`
workflow then runs, in order:

- **`llz ci bao-configure`** — a `keycloak` JWT/OIDC auth mount (issuer derived
  from the region's `cluster.bootstrap.domainSuffix` → the `otomi` realm), a
  `gsap-writer` policy, and a `keycloak` role bound on the OIDC `groups` claim
  value **`team-gsap`** (the apl-core realm role). Uses the root token bootstrap
  already holds — no manual root step.
- **`llz ci keycloak-configure`** — the one realm object apl-core does *not*
  ship: a public **device-flow** client (`llz`) carrying the default `openid`
  scope (which already emits the `groups` claim). Best-effort: a Keycloak failure
  warns and falls back to the manual step below; it never wedges the bootstrap.

Skip to *Onboard a user*.

## Retrofit path (existing clusters): declare, render, then configure by hand

Existing instances have no team until you add one (there is no automatic
default on upgrade — new clusters get theirs at `llz new`). Declare a team in
`landingzone.yaml` (steps below use `gsap`); an already-bootstrapped cluster
revoked its root token, so:

```bash
# 1. Declare the team (above), then render + commit so apl-core makes team-<name>:
llz render && git commit -am "feat: add gsap team" && git push   # apl-core converges the group/role

# 2. OpenBao side (needs root) + the device-flow client:
export OPENBAO_ROOT_TOKEN=<root>     # llz openbao regen-root <region> if revoked
llz ci bao-configure --region <region>       # keycloak mount + gsap-writer policy + role
llz ci keycloak-configure --region <region>  # public device-flow `llz` client
```

All idempotent. `bao-configure` is the only step that needs root. If
`keycloak-configure` can't reach Keycloak, create the client by hand: a **public**
client (default id `llz`) with **OAuth 2.0 Device Authorization Grant** enabled
and the **`openid`** default scope. The `team-<name>` group and the `groups`
claim come from apl-core — nothing to do there.

## Onboard a user + let them write (per user / per session)

- **Platform admin:** add the person as a realm user and put them in the
  **`team-gsap`** group (Keycloak → Groups → team-gsap → Members, or the group tab
  on the user). This per-user membership is the one step that stays manual (unless
  membership is driven from an external IdP).
- **Operator** (needs only kubectl reach to the cluster — the shared kubeconfig
  today; a scoped one in Phase 2):

  ```bash
  eval "$(llz openbao login --team gsap)"     # browser device login → OPENBAO_TOKEN
  llz openbao set secret/gsap/build/yakpurger \
      username=x-access-token token=<pat> --yes
  ```

`login` runs the device flow, then port-forwards to OpenBao and exchanges the
id_token for a token carrying only `gsap-writer`. The write is **attributed**
(the Keycloak `sub`) and **least-privilege** (the `secret/gsap/*` subtree only) —
no root, no broad token. A token that tries to write outside its subtree gets a
403.

## Validate the whole chain (browser-free smoke)

`llz ci team-login-smoke --region <region> [--team <name>]` validates the entire
path end-to-end **without a browser**: it provisions a throwaway Keycloak user in
the `team-<name>` group, mints an id_token via a temporary direct-grant client
(the same `groups` claim the device flow carries), exchanges it at OpenBao's
`keycloak` mount, then asserts a write to the team's subtree **succeeds** and a
write to `secret/linode/*` is **denied (403)** — tearing the user + client down
after. It exercises exactly the apl-core-dependent wiring (group → realm role →
`groups` claim → OpenBao role → scoped policy).

Requirements: cluster access (reads the admin creds + port-forwards OpenBao), a
**publicly-resolvable** `keycloak.<domainSuffix>` (so the id_token's `iss` matches
OpenBao's configured issuer), and a **converged** apl-core (the `team-<name>`
group must exist). Meant for the e2e lane or a manual post-bootstrap check; it
does not drive the device-flow browser UI (that's generic OAuth 2.0 device grant,
covered by unit tests).

> **Note — the smoke leaves litter.** Its write-succeeds assertion writes a real
> `_llz_smoke_<timestamp>` key under the team subtree, and the smoke token has no
> `delete` capability (by design — writers can't delete), so each run **accumulates**
> one such key. Fine on an e2e/throwaway cluster; on a long-lived cluster, prune them
> with the root token (`bao kv metadata delete secret/<subtree>/_llz_smoke_<ts>`) if
> they pile up.

## Offboard a team (remove write access)

Everything in this feature is **additive** — removing a team from `spec.teams` and
re-rendering **revokes nothing** (the committed `teamConfig.<name>` round-trips, so
apl-core keeps the Keycloak group; `bao-configure` only upserts, so the role + policy
persist). Members keep mintable scoped write access until you tear it down explicitly.
To fully offboard team `<name>` (needs the root token):

```bash
export OPENBAO_ROOT_TOKEN=<root>   # llz openbao regen-root <region> if revoked
# 1. OpenBao: remove the login role + the writer policy.
bao delete   auth/keycloak/role/<name>
bao policy delete <name>-writer
# 2. apl-core: HAND-DELETE the teamConfig.<name> entry from the committed
#    apl-values/<env>/values.yaml (render only ADDS/preserves teamConfig — dropping the
#    team from spec.teams stops it being re-added but removes NOTHING already committed).
#    Then drop it from spec.teams too, `llz render`, and commit. With teamConfig.<name>
#    gone from values, apl-core deletes the team-<name> Keycloak group/role + namespace.*
# 3. Revoke the root token again when done (llz ci bao-breakglass --action revoke, or manually).
```

Removing just the role+policy (step 1) is the immediate lockout; step 2 cleans up the
identity side. Until step 1 runs, offboarding is incomplete.

> \* **E2E-gated:** that apl-core actually deletes a team when its `teamConfig` entry
> disappears is unvalidated on self-installed apl-core (the same apl-core-shape
> assumption `team-login-smoke` exercises for the add path). Verify on the e2e cluster
> before relying on step 2's cleanup; step 1 is the guaranteed access removal regardless.

## Troubleshooting

- **`login` says the realm lacks a device endpoint** — the device-flow client
  isn't enabled, or `--client-id` is wrong.
- **`oidc auth login … returned no client_token` / 403** — the id_token's
  `groups` claim doesn't carry **`team-<name>`** (user not in the `team-<name>`
  group, or the team never converged in apl-core). Confirm group membership and
  that `teamConfig.<name>` rendered + apl-core created the group/role.
- **`no device_authorization_endpoint` via discovery** — check the issuer
  (`--issuer https://keycloak.<domain>/realms/otomi`) resolves and is reachable.
- **the exchange hangs / `no client_token` even though the browser login worked**
  — OpenBao validates the token by fetching Keycloak's JWKS, and it does so over
  the **internal** service (`keycloak-keycloakx-http.keycloak.svc:8080`), not the
  public URL (which hairpins off the cluster's own LB). If that times out, the
  `llz-openbao` → `keycloak` NetworkPolicy egress allow is missing (chart value
  `platform.networkPolicy.keycloakNamespace`) or the mount was configured with the
  public `oidc_discovery_url` instead of the internal `jwks_url` — re-run
  `llz ci bao-configure`.
- **login 403s right after a fresh bootstrap** — `keycloak-configure` waits up to
  ~5 min for apl-core's `openid` scope; if apl-core converged Keycloak later than
  that, the `llz` client may lack the groups claim. The **weekly scheduled-checks
  workflow re-runs `keycloak-configure` and self-heals this** (it reconciles the
  scope onto the existing client, idempotently). To fix it immediately instead of
  waiting for the weekly run, run `llz ci keycloak-configure --region <region>`.
