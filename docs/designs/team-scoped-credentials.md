# Design: team-scoped credentials — retire the shared admin kubeconfig and root-for-writes

**Status:** Proposed. Phase 1 (Keycloak → OpenBao write identity) is buildable
now with no LKE-E control-plane dependency; Phase 2 (scoped kube credentials) is
gated on an open LKE-Enterprise feasibility question (see *Open question*).

Related: [`docs/adr/0004-decouple-openbao-write-identity-from-cluster-access.md`](../adr/0004-decouple-openbao-write-identity-from-cluster-access.md),
[`docs/runbooks/lke-admin-rotation.md`](../runbooks/lke-admin-rotation.md),
[`tools/cmd/llz/ci_openbao_configure.go`](../../tools/cmd/llz/ci_openbao_configure.go),
[`tools/cmd/llz/openbao.go`](../../tools/cmd/llz/openbao.go).

## Problem

There is exactly **one** human-facing credential per LKE-Enterprise cluster: the
Linode-issued **cluster-admin** kubeconfig (`lke-admin-token`), fetched via the
Linode API or Terraform state
([`fetchkubeconfig.go:9`](../../tools/cmd/llz/fetchkubeconfig.go),
[`credentials_lkeadmin.go:45`](../../tools/cmd/llz/credentials_lkeadmin.go)) and
shared by every SRE and every CI job
([`lke-admin-rotation.md:13`](../runbooks/lke-admin-rotation.md),
[`prom_query.go:8`](../../tools/cmd/llz/prom_query.go)). It is
`system:masters`-equivalent, unattributable (everyone is the same identity),
and can only be rotated by deleting and regenerating that single token — never
scoped or individually revoked.

That single identity sits at the bottom of **two** access paths:

1. **Cluster access** — anyone with the kubeconfig is cluster-admin.
2. **Secret writes** — `llz openbao get|set` reaches OpenBao *through* that
   kubeconfig (kubectl `port-forward` / `exec`, since OpenBao has no external
   ingress). Broad writes require the **root token**, which every bootstrap
   deliberately revokes
   ([`ci_openbao_configure.go:358`](../../tools/cmd/llz/ci_openbao_configure.go)),
   so the documented operator flow is `regen-root` (a 3-of-5 unseal-key quorum)
   before every write session.

The result: routine secret writes require reconstituting root, and there is no
per-team attribution or least-privilege anywhere in the human path.

## Key insight — these are separable problems

Cluster access and OpenBao write authority are coupled only by *network
transport*, not by *authorization*:

- The kubeconfig is needed to **reach** OpenBao (port-forward), because OpenBao
  has no external ingress.
- The kubeconfig is *not* inherently needed to **authorize** a write — OpenBao's
  own auth methods decide that, and **we run OpenBao**. Its JWT/OIDC auth is
  already used for GitHub Actions OIDC
  ([`ci_openbao_configure.go:284`](../../tools/cmd/llz/ci_openbao_configure.go));
  pointing a second mount at Keycloak needs no LKE-E cooperation.

Therefore: **root-for-writes can be eliminated without solving the LKE-E
kubeconfig problem.** The shared kubeconfig stays as the port-forward transport;
it stops being the write *authority*. This is Phase 1 — the majority of the
value — and it carries no managed-control-plane risk.

## Identity model — teams are Keycloak groups

APL/Otomi already ships a non-disableable **standalone Keycloak** in the `otomi`
realm, used today only as the browser-SSO IdP for platform web UIs via
oauth2-proxy — it is **not** wired to the kube-apiserver. We reuse that realm as
the identity source: an **APL team is a Keycloak group** (e.g. `gsap`). One human
login backs both halves of the plan; no new IdP is introduced.

## Phase 1 — Keycloak-backed OpenBao write identity (no LKE-E dependency)

Extend [`ci_openbao_configure.go`](../../tools/cmd/llz/ci_openbao_configure.go),
whose policy/role sequence is already idempotent and root-driven at bootstrap:

1. **Second JWT/OIDC auth mount** `oidc-keycloak`, with
   `oidc_discovery_url = https://keycloak.<domainSuffix>/realms/otomi`. This
   mirrors the existing GitHub-OIDC `jwtRole` closure
   ([`:274`](../../tools/cmd/llz/ci_openbao_configure.go)); the GitHub mount is
   untouched.
2. **Per-team write policy**, shaped like the existing narrow allowlists
   ([`:74-104`](../../tools/cmd/llz/ci_openbao_configure.go)). Example
   `gsap-writer`:
   ```hcl
   path "secret/data/gsap/*"     { capabilities = ["create","update","read"] }
   path "secret/metadata/gsap/*" { capabilities = ["read","list"] }
   ```
3. **Role bound on the group claim**: `bound_claims = {"groups":"gsap"}`,
   `user_claim = "sub"`, `token_ttl = 15m`, policy `gsap-writer`. A team's secret
   subtree and its Keycloak group are declared together (see *Spec surface*).
4. **`llz openbao login`** — a Keycloak **device-code flow** (`keycloak.<suffix>`
   is externally reachable) → id_token → `auth/oidc-keycloak/login` on OpenBao,
   reached over the **existing auto port-forward** from PR #298 → caches a
   short-lived, team-scoped token in the shell. Subsequent `llz openbao set`
   picks it up via the normal `OPENBAO_TOKEN` path
   ([`openbao.go`](../../tools/cmd/llz/openbao.go)).

**Outcome:** operators authenticate as themselves; writes are **attributed** (the
`sub` claim) and **least-privilege** (their team subtree only); root stays
revoked. The interim workaround until this lands is a one-time-root-minted
periodic token stored as `OPENBAO_TOKEN` — Phase 1 replaces that hand-minted
token with a Keycloak-brokered one.

## Phase 2 — scoped kube credentials per team (LKE-E-constrained)

Retiring the shared admin kubeconfig itself. The design forks on the *Open
question* below:

- **If the LKE-E apiserver can consume a custom OIDC issuer** → issue real
  per-user OIDC kubeconfigs against the same Keycloak realm; map the group claim
  to scoped `Role`/`RoleBinding`s (a per-team RBAC generator in `render`, which
  emits none today). Cleanest outcome — one identity, both planes.
- **If it cannot** (the expected result for a managed control plane) → per-team
  **ServiceAccount + scoped RBAC**, issued as short-lived **bound
  ServiceAccount-token kubeconfigs** (TokenRequest API) by a new
  `llz credentials team-kubeconfig`, ideally brokered behind the oauth2-proxy /
  Keycloak that already fronts the platform so issuance is itself group-gated.

**The minimum that connects the phases:** a team SA needs only `pods/portforward`
+ `get pods` in the `llz-openbao` namespace to *reach* OpenBao — a tiny `Role`,
no cluster-admin. That plus the Phase-1 token yields a complete team-scoped
secret-write path with **neither** the shared admin kubeconfig **nor** root.
Broader team kube access (operating workloads, namespaces) is a separate, larger
RBAC effort layered on the same identity.

Phase 2 also inherits two LKE-E realities to design around: the control-plane
**IP-ACL** gating (`cluster-access` opens the runner egress IP;
[`cluster-access/action.yml`](../../instance-template/.github/actions/cluster-access/action.yml))
and admission **webhook denials** on some subresources for even cluster-admin
(`services/proxy` denied, `pods/portforward` allowed —
[`prom_query.go:6`](../../tools/cmd/llz/prom_query.go)).

## Spec surface

Teams are greenfield in the spec — the LandingZone CRD has no `Team` type today
([`clusterspec/types.go`](../../tools/internal/clusterspec/types.go)). Introduce a
minimal declaration so team → group → secret-subtree is one source of truth that
`ci bao-configure` (and, in Phase 2, an RBAC generator) consumes:

```yaml
spec:
  teams:
    - name: gsap
      keycloakGroup: gsap          # defaults to name
      openbaoSubtree: secret/gsap  # → gsap-writer policy on secret/data/gsap/*
```

## Open question (decides Phase 2, blocks nothing in Phase 1)

**Can the LKE-Enterprise managed apiserver accept a custom OIDC issuer** — via
`--oidc-*` flags or a Structured Authentication Config file? There is no such
wiring anywhere in the repo today, and managed control planes typically do not
expose it. The answer (from Akamai/Linode docs or support) determines whether
Phase 2 is "real OIDC kubeconfigs" or "brokered SA tokens." Phase 1 is unaffected
either way.

## Non-goals

- CI / in-cluster automation identity — already solved (GitHub-OIDC jwt roles +
  Kubernetes-auth roles); this design is scoped to **human operators**.
- External app-team tenancy (teams deploying their own workloads) — a later layer
  on the same Keycloak-group identity.
- Giving OpenBao an external ingress — out of scope; the port-forward transport
  stays.
