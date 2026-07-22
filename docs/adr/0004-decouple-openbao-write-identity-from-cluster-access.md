# ADR 0004 — Decouple OpenBao write identity from the shared LKE-E kubeconfig

- Status: Accepted
- Date: 2026-07-21
- Deciders: platform / LLZ maintainers
- Related: [`docs/designs/team-scoped-credentials.md`](../designs/team-scoped-credentials.md),
  [`docs/runbooks/lke-admin-rotation.md`](../runbooks/lke-admin-rotation.md),
  [`tools/cmd/llz/ci_openbao_configure.go`](../../tools/cmd/llz/ci_openbao_configure.go)

## Context

LKE-Enterprise issues exactly **one** human-facing credential per cluster: the
Linode-issued cluster-admin kubeconfig (`lke-admin-token`), shared by every SRE
and CI job and rotatable only by delete-and-regenerate. That single identity
backs two access paths: cluster-admin on the apiserver, and — because
`llz openbao` reaches OpenBao by kubectl `port-forward`/`exec` (OpenBao has no
external ingress) — broad secret writes, which require the **root token** that
every bootstrap deliberately revokes. Routine writes therefore mean
reconstituting root via a 3-of-5 unseal-key quorum, with no per-team attribution
or least privilege anywhere in the human path.

The desired end state is **per-APL-team credentials** for human operators, with
those team identities also granted **scoped** OpenBao write access, retiring
root-for-everything. APL already ships a standalone Keycloak (`otomi` realm) as
the platform SSO IdP, so teams can be modeled as Keycloak groups — but Keycloak
is **not** wired to the kube-apiserver, and on a managed LKE-E control plane it
is unclear whether it can be.

## Decision

**Split the goal along the authorization boundary, and do the OpenBao half
first.**

1. **OpenBao write authority is decoupled from cluster access.** The shared
   kubeconfig remains only the *transport* that port-forwards to OpenBao; it
   stops being the *authority* for writes. OpenBao's own auth decides that, and
   we run OpenBao.

2. **Team identity for OpenBao = Keycloak group, via a second OIDC auth mount.**
   `ci bao-configure` gains a `keycloak` JWT/OIDC mount
   (`oidc_discovery_url = https://keycloak.<domainSuffix>/realms/otomi`) plus a
   per-team write policy and a role bound on the `groups` claim — mirroring the
   existing GitHub-OIDC jwt roles and narrow policies. Operators
   `llz openbao login` (Keycloak device-code flow) for a short-lived,
   team-scoped token. **Phase 1; ships independently of any LKE-E change.**

3. **Scoped kube credentials are Phase 2, gated on an LKE-E feasibility
   question** — whether the managed apiserver can consume a custom OIDC issuer.
   If yes: real OIDC kubeconfigs + group→RBAC. If no: per-team ServiceAccounts +
   scoped RBAC issued as short-lived bound-token kubeconfigs. Either way the
   connecting minimum is a team SA with only `pods/portforward` in `llz-openbao`,
   which together with the Phase-1 token gives a full team-scoped write path with
   neither the shared admin kubeconfig nor root.

Alternatives considered and rejected:

- **Wait for a unified apiserver-OIDC solution before doing anything.** Couples
  the achievable win (killing root-for-writes) to an unresolved managed-control-
  plane constraint. Rejected — the two are separable and Phase 1 has no such
  dependency.
- **Keep minting scoped OpenBao tokens from root by hand.** Works, but every
  write session still reconstitutes root (quorum), and tokens are unattributed
  bearer creds in a file. Acceptable only as the pre-Phase-1 interim.
- **Give OpenBao an external ingress so laptops address it directly.** Enlarges
  the attack surface of the platform's secret store to solve an ergonomics
  problem the port-forward transport already covers. Rejected.

## Consequences

- Root-for-writes is eliminated for human operators once Phase 1 lands; writes
  become attributed (Keycloak `sub`) and least-privilege (team subtree only).
- A new dependency: the OpenBao write path now relies on Keycloak being healthy
  and the operator being a member of the right realm group. Keycloak is already a
  non-disableable core app, so this adds no new component — but it does add
  Keycloak to the secret-write blast radius.
- `spec.teams` (team → group → secret subtree) becomes the single source of truth
  `ci bao-configure` consumes, and that Phase 2's RBAC generator will reuse.
- The managed-apiserver-OIDC question is documented as the explicit gate for
  Phase 2; nothing in Phase 1 is blocked on it.

## Implementation notes (as-built — Phase 1 shipped in #300)

The decision above stands; three specifics evolved during implementation and
review, and the shipped code is authoritative where it diverges:

- **Teams reuse apl-core's native teams, not a bespoke Keycloak group.**
  Declaring a `spec.teams` entry makes `llz render` emit a `teamConfig.<name>`
  overlay so apl-core provisions the team (namespace + realm group + realm role
  **`team-<name>`** + a `groups`-claim mapper). The OpenBao role therefore binds
  on the `groups` value `team-<name>` (not a bare team name). Consequently the
  spec surface is just `{name, openbaoSubtree}` — the `keycloakGroup` field in
  the design's first sketch was dropped (the group is deterministic).
- **The `keycloak` mount validates via the internal JWKS, not the public
  discovery URL.** `oidc_discovery_url = https://keycloak.<domainSuffix>/…`
  hairpins on LKE-E (the public host resolves to the cluster's own LB IP), so the
  mount is configured with `jwks_url` (Keycloak's in-cluster http service) +
  `bound_issuer` (the public issuer, matching the token `iss`), plus
  `skip_jwks_validation=true` so the config write doesn't eagerly fetch before
  Keycloak has converged. An egress NetworkPolicy allows openbao → keycloak.
- **Tokens are audience-bound.** The role sets `bound_audiences=[llz]` and
  `keycloak-configure` stamps an `aud:llz` mapper on the login client, so only a
  token deliberately minted for OpenBao login is accepted — not any otomi-realm
  id_token that merely carries the `groups` claim.

See [`tools/cmd/llz/ci_openbao_configure.go`](../../tools/cmd/llz/ci_openbao_configure.go),
[`tools/cmd/llz/ci_keycloak_configure.go`](../../tools/cmd/llz/ci_keycloak_configure.go),
and [`docs/runbooks/openbao-team-login.md`](../runbooks/openbao-team-login.md).
