# ADR 0005 — Pivot LLZ to Linode Managed App Platform (`apl_enabled`)

Status: **Accepted — managed is the ONLY mode.** The pivot is complete: LLZ no
longer self-installs apl-core. `cluster.bootstrap.managedAppPlatform: true` is
MANDATORY (validation rejects a self-install spec), `linode_lke_cluster.apl_enabled`
is always true, and the self-install bootstrap path, its value-render/otomi.git-seed
pipeline, the `manifest`↔`manifest-managed` split, the `certAutomation` component,
and the per-component managed-tier table have all been REMOVED — the managed
disposition now lives on the component registry itself (`Component.ManagedSkip` /
`ManagedConditionalOn`). There are no production clusters and no upgrade path, so no
migration was needed. The sections below are retained as the design record; where
they describe a self-install path or an opt-in toggle, read that as historical — it
no longer exists.

**Corrections — where the shipped code diverged from the design log below (authoritative; the prose further down predates these):**
- **No opt-in flag.** `managedAppPlatform: true` is mandatory; there is no `--managed-app-platform` toggle and no self-install default. Phase 1's "default off = unchanged self-install" framing is obsolete.
- **Block-storage is the cluster DEFAULT on managed** (same as self-install), not the *non-default* class GAP 1 describes. The always-on `llzReconciler` sc-demote pass keeps LKE's `linode-block-storage-retain` non-default, so `block-storage-retain` is the single encrypted+Retain default. `managedBlockStorageClassYAML` was dropped.
- **The manifest tree was collapsed to ONE base** — there is no `manifest-managed` variant (as the status line above states). GAP 2's "Managed base variant `platform-apl/manifest-managed/`" prose predates the collapse; the self-install-only pieces were dropped from the single base instead.
- **Values ownership was reversed by [ADR 0006](0006-managed-default-apps.md).** The "managed apl-core owns its values via apl-api + in-cluster gitea; LLZ does NOT push into apl-core's values/gitea" conclusion below (Option A, spike findings) is **superseded**: LLZ *does* push a github `apl-<env>` values branch (App Platform BYO-Git) to enable apl-core's default apps and repoints apl-core at it. See ADR 0006 for the shipped mechanism.

Date: 2026-07-22
Supersedes/relates: the DNS/portal thread on gsap-apl-prod; blocks the full "LLZ clusters get the akamai-apl.net portal" requirement.

## Context

LLZ today **self-installs apl-core** (`llz ci bootstrap-cluster` runs `helm upgrade --install apl` + seeds the `otomi.git` values branch). That is a deliberate choice for control. But it means LLZ clusters are **not** the managed "Application Platform for LKE" product, so they do **not** get what makes the portal "just work":

- The **`lke<clusterID>.akamai-apl.net` domain**, provisioned in **Akamai Edge DNS** (nameservers `*.akam.net`).
- Automatic **DNS records** (`*.lke<id>`) → the cluster ingress.
- A **wildcard TLS cert** (Let's Encrypt via Akamai Edge DNS-01).
- Registration in the Linode Cloud Manager "App Platform" section (portal URL surfaced).

Investigation (against a live managed cluster `lke579582` vs the LLZ cluster `lke633340`) confirmed:
- `akamai-apl.net` is an Akamai-managed Edge DNS zone; an LLZ cluster cannot write to it.
- LLZ's DNS design targets **Linode DNS** (`dns.provider.linode`), not Akamai Edge DNS.
- On `gsap-apl-prod`, `domainSuffix` was even mis-set to *another cluster's* managed domain — the portal was doubly broken (wrong domain + self-signed cert). No LLZ change caused this; it exposed the gap.

**The provisioning mechanism** is a single Terraform lever, verified in the Linode provider source
(`linode/lke/schema_resource.go`): `linode_lke_cluster.apl_enabled` (bool, **v4beta/enterprise only, `ForceNew`**).
Setting it makes Linode install+manage apl-core **and** provision the domain/DNS/cert. It is **coupled** —
there is no domain-only field and no `dashboard_url`/domain output attribute.

## Decision

**Pivot LLZ from "self-install apl-core" to "use + extend Linode's managed App Platform"**, opt-in per cluster via
`spec.cluster.bootstrap.managedAppPlatform` → `apl_enabled=true`. Linode owns the apl-core lifecycle + the
akamai-apl.net domain; LLZ **layers its customizations on top** (apl-values overrides, OpenBao, harbor wiring,
team-scoped credentials) rather than owning the base install.

Rationale: it is the *only* way to get the akamai-apl.net portal (domain + DNS + wildcard cert + Cloud-Manager
registration) programmatically, and it removes a large class of DNS/cert/reachability problems LLZ has been fighting.

### Trade-offs
| Gain | Cost |
|---|---|
| Built-in `akamai-apl.net` domain + DNS + wildcard cert (the portal) | Linode owns apl-core version/lifecycle — LLZ loses "pin our own apl-core" |
| Portal shows in Cloud Manager; users authenticate + use the web-UI | `apl_enabled` is `ForceNew` — set at cluster **creation**; existing clusters must be recreated |
| Much less apl-core plumbing for LLZ to maintain | The domain is **not** a TF output — LLZ must discover it post-install |
| Custom app domains still work (apl-core keeps platform vs per-team domains separate) | Downstream (bootstrap-openbao, apl-values render, every `domainSuffix`-derived value) must be reworked to *discover* the domain and *layer onto* a managed apl-core |

## Phased plan (each phase validated by an e2e/spike iteration)

- **Phase 1 — enablement + install-skip (DONE).**
  - `spec.cluster.bootstrap.managedAppPlatform` (bool) → tfvar `apl_enabled` (`ClusterTFVars`).
  - `terraform-modules/llz-cluster` + cluster tfroot: `apl_enabled` variable → `linode_lke_cluster.apl_enabled`.
  - `llz ci bootstrap-cluster --managed-app-platform`: **early-skip** the self-install (no `helm install apl`,
    no branch seed) so it can't collide with the managed install. Default off = unchanged self-install.
  - Enough to run an e2e/throwaway `apl_enabled=true` cluster and **learn** the managed behavior.

- **Phase 2 — domain + values discovery (NEXT, driven by the spike).**
  - Discover the managed domain (`lke<id>.akamai-apl.net`) — from where? (apl-core config CR? a Linode API? the
    ingress?) — and thread it into everything that currently derives from `domainSuffix`
    (`llz ci resolve-harbor-url`, OpenBao issuer, etc.).
  - Determine how managed apl-core manages values (does it use an `otomi.git` branch like self-install? does LLZ
    still push apl-values, or overlay differently?). Adapt `llz render` / the apl-values overlay accordingly.

- **Phase 3 — DNS/cert + component ownership.**
  - Managed apl-core owns external-dns (Akamai Edge DNS) + the wildcard cert. Remove LLZ's Linode-DNS wiring on
    managed clusters; keep the door open for **custom app domains** on top.

- **Phase 4 — the LLZ extras on a managed cluster.**
  - Re-validate OpenBao bootstrap, harbor, the reconciler, and the team-scoped-credentials work (PR #300) against
    a managed apl-core. (Team-creds reachability, etc., should behave the "normal managed" way — re-test.)

## Open questions (for the human) — RESOLVED

All five were answered by the spike + Phase-4 findings below and by [ADR 0006](0006-managed-default-apps.md): (1) the domain is `lke<id>.akamai-apl.net`, read from `otomi/otomi-api`'s `SSO_ISSUER`; (2) values are driven via BYO-Git (ADR 0006); (3) apl-core v6.0.0 = LLZ baseline; (4) new-clusters-only, no migration; (5) node sizing captured at the end of this ADR. Retained for the record:

1. **Domain discovery:** where does the `lke<id>.akamai-apl.net` domain surface for programmatic read (no TF
   output)? apl-core config / a Linode API / the ingress host?
2. **Values ownership:** does managed apl-core expect the operator to push values (git), or is it Cloud-Manager /
   API driven? How does LLZ inject its overlays?
3. **apl-core version:** managed pins its own apl-core version — is that acceptable vs LLZ's baseline pin?
4. **Existing clusters:** `apl_enabled` is ForceNew → recreate. Migration story for already-deployed LLZ clusters?
5. **Node sizing:** does managed APL impose node minimums we must encode in the spec defaults?

## Spike findings (live managed `apl_enabled` cluster, LKE id 634445 — CONFIRMED)

Validated against a real managed cluster (created via Cloud Manager; apl-core **v6.0.0** — matches LLZ's baseline).
**The pivot delivers the full portal experience.**

- **Domain = `lke<clusterID>.akamai-apl.net`, DETERMINISTIC** (cluster 634445 → `lke634445.akamai-apl.net`). Every
  platform service has an HTTPRoute there: `console`, `keycloak`, `api`, `argocd`, `grafana`, `prometheus`, `harbor`,
  `git`, `auth` (oauth2-proxy), `tty`, plus `*.lke<id>.akamai-apl.net`. → **Phase-2 domain discovery is trivial:
  derive it from the LKE cluster id** (robust fallback: read a platform HTTPRoute hostname, or the `otomi/otomi-api`
  ConfigMap where `domainSuffix` lives).
- **DNS: Akamai Edge DNS, auto-provisioned** — `console.lke634445.akamai-apl.net` resolves publicly (172.237.158.144 =
  the cluster ingress). LLZ does nothing.
- **Cert: valid public Let's Encrypt** (`ssl_verify=0`, issuer `Let's Encrypt YR1`) — portal is browser-trusted.
  (In-cluster there's a `custom-ca` ClusterIssuer for internal traffic; the public LE cert is edge/platform-provided.)
  → On managed clusters LLZ must **NOT** wire Linode-DNS external-dns or cert-manager Let's Encrypt — the platform owns it.
- **Portal works:** `console.lke634445.akamai-apl.net` → 403 (auth required = live); `keycloak…` → 302. `KC_HOSTNAME`
  is the managed domain. Users authenticate via the platform Keycloak, exactly as on a managed cluster.
- **Values ownership (the real Phase-2 design question):** managed apl-core owns its values via the **apl-api**
  (`otomi/otomi-api` deployment + `otomi-api`/`otomi-api-core` ConfigMaps) backed by an **in-cluster gitea**
  (`git-server`/`gitea` namespaces), managed through the console. There is **no external `otomi.git` branch LLZ pushes
  to** (unlike self-install). So LLZ can't inject its overlays the old way — it must either install its extras
  (OpenBao, harbor-robot, reconciler, team-creds) as **separate Argo apps layered on top**, or drive the apl-api.

### Refined Phase-2/3 plan (informed)
1. **Domain/issuer discovery (DONE for the runtime consumers):** rather than re-derive the domain, read apl-core's
   **own** config as the source of truth. The `otomi/otomi-api` ConfigMap carries `SSO_ISSUER =
   https://keycloak.lke<id>.akamai-apl.net/realms/otomi` (and `SSO_JWKS_URI`) — exactly the value the team-creds
   OpenBao OIDC mount needs. Implemented `discoverKeycloakIssuerFromCluster()` (reads that CM key) and a shared
   `discoverManagedDomain()` (`managedDomainFromIssuer` strips the issuer to the bare `lke<id>.akamai-apl.net`).
   Routed through it **on managed clusters** (`managedAppPlatform: true`, spec `domainSuffix` empty): `keycloakIssuerFor`
   (bao-configure team OIDC), `llz ci resolve-harbor-url` (→ `harbor.<managed-domain>`), and the keycloak team-login
   smoke test (→ `keycloak.<managed-domain>`). All three run with the bootstrap kubeconfig, so discovery is available;
   each degrades to its existing spec/override/error path when unreachable. This closes the "no spec domainSuffix →
   host unresolvable" gap those commands would otherwise hit on managed.

   **Render-time caveat (→ Phase 4):** `llz render` runs in CI with NO cluster access, so `RenderValues` /
   `RenderHarborHostPatch` (which bake `harbor.<domainSuffix>` into the in-cluster harbor-robot-provisioner's
   `HARBOR_HOST`) cannot discover the managed domain the same way. On managed either (a) the in-cluster provisioner
   discovers the domain itself at runtime (preferred — keeps render domain-agnostic), or (b) a post-provision step
   writes the discovered domain into the instance vars/spec before render. Decide + validate with a live managed
   cluster in Phase 4. (Note: on managed, LLZ does NOT render apl-core's `values.yaml` at all — the managed
   bootstrap path skips value rendering — so the `otomi.hasExternalDNS` / `dns.provider.linode` values wiring is
   already moot there; only the extras' render-time host injection remains.)
2. **DNS/cert (→ Phase 4, needs live validation):** on managed, skip LLZ's Linode-DNS + cert-manager wiring — the
   platform owns external-dns (Akamai Edge DNS) + the public wildcard cert, and already ships a `custom-ca`
   ClusterIssuer. The apl-core **values** wiring is moot on managed (no value render). What remains is whether the
   *extras* manifest tree (layered via the Argo bridge) carries cert-manager/DNS resources that conflict with managed
   apl-core's own — determine against a live cluster before gating anything off (speculative gating risks breaking the
   OpenBao bootstrap-CA chain).
3. **Values/extras (option A — chosen, BUILT + VALIDATED END-TO-END):** managed apl-core owns its base values via
   apl-api + in-cluster gitea (no external `otomi.git` branch to push to), so LLZ layers its extras (OpenBao,
   harbor-robot, reconciler, team-creds) as **separate Argo Applications on top** of the managed install — it does
   **not** push into apl-core's values/gitea. Implemented: `runBootstrapCluster` no longer early-returns on
   `managedAppPlatform`; it calls `runBootstrapClusterManaged`, which waits for the managed ArgoCD
   (`waitManagedArgoReady`: Application CRD + argocd-server) then applies ONLY the Argo bridge (the same
   `platform-bootstrap` AppProject + Application + `llz-secret-store` Application the self-install uses at step 10) —
   skipping the apl-core helm install, otomi.git seed, value render, DNS/cert, and Kyverno (all Linode's on managed).

### Option-A validation (live managed cluster lke634487 — apl-core v6.0.0, CONFIRMED)
Provisioned via a minimal Terraform config (see the provisioning note below) and driven with the real
`llz ci bootstrap-cluster --managed-app-platform`:
- **Managed ArgoCD posture:** `application.namespaces` is EMPTY → it watches only the `argocd` namespace for
  Applications, which is exactly where the bridge places them. `resourceTrackingMethod=annotation`. AppProjects are
  just `default` + `team-admin`; apl-core's own apps live in `default`, so adding LLZ's `platform-bootstrap` project
  doesn't collide. No admission webhook blocked creating an external AppProject/Application.
- **Probe:** an external LLZ-shaped AppProject + Application (guestbook) went **Synced/Healthy in ~10s**, workloads
  Running. apl-core's own 38 apps stayed Healthy throughout.
- **Real command, end-to-end:** `--managed-app-platform` waited for ArgoCD, applied the bridge, and created
  `platform-bootstrap` (AppProject + Application) and `llz-secret-store` in the managed `argocd` ns. `llz-secret-store`
  (which sources `platform-apl/manifest-secret-store` from the template repo) went **Synced** — a genuine LLZ manifest
  tree pulled + applied by managed ArgoCD (Degraded only because its OpenBao backend isn't deployed yet).
  `platform-bootstrap` is ComparisonError, as expected when pointed at a non-rendered instance repo (the
  `apl-values/<env>/manifest` path exists only in a rendered instance). apl-core's 38 apps + the portal stayed intact.
- **Conclusion:** the existing self-install Argo bridge is a drop-in for managed apl-core. Option A needs no new
  layering machinery — only the `managedAppPlatform` branch that applies the bridge and skips the apl-core install.

### Provisioning note (raw API vs Terraform) — IMPORTANT
`apl_enabled` (enterprise) clusters **cannot be provisioned with a bound VPC via the raw v4beta API**: the create
silently drops the VPC binding (`vpc_id`/`subnet_id` return null, zero nodes) no matter where `subnet_id` is placed,
and `apl_enabled` clusters do NOT auto-provision their own VPC either (that's Cloud Manager's doing). Without a VPC,
LKE-E never provisions nodes. The **Terraform `linode/linode ~> 3.11` provider binds it correctly** (nodes came up
Ready, kubectl reachable via the control-plane ACL). So LLZ's TF path is the only supported provisioning route — which
is fine, since that's how LLZ provisions anyway. (Side note: managed LKE-E assigns node internal IPs from its own
range, not the subnet CIDR — the `vpcSubnetCIDR` spec field is effectively advisory on managed clusters. The provider
also throws a benign post-create `failed to list firewall rulesets [404]` on apl_enabled clusters.)
4. **Version:** none needed — managed is v6.0.0 = LLZ baseline.

## Phase-4 coexistence findings (live managed cluster lke634506 — CONFIRMED)

Ran a read-only inspection + targeted validation against a fresh managed cluster to check whether the LLZ extras
(built for *self-installed* apl-core) coexist with *managed* apl-core, which already runs its own ESO / cert-manager /
storage / NetworkPolicies. Results:

**Green — no fix needed:**
- **NetworkPolicies:** managed imposes only a few TARGETED NPs (gitea, otomi-api, oauth2-redis), NOT a blanket
  default-deny. The extras' namespaces (llz-openbao, harbor, keycloak, external-secrets) are NP-free → cross-namespace
  ESO→OpenBao→keycloak→harbor traffic is NOT blocked. (This retires the biggest suspected risk — and the old
  hostname-strict/hairpin OpenBao→Keycloak saga.)
- **Keycloak internal JWKS:** bao-configure's hardcoded `keycloak-keycloakx-http.keycloak.svc:8080/realms/otomi/…/certs`
  returns HTTP 200 on managed (service name + port + path all match; no Host header needed). An initial 404 was purely
  realm-not-ready timing. Team-creds OIDC works on managed.
- **ESO / sealed-secrets / cert-manager / prometheus-operator:** all present with the CRD kinds the extras use
  (clustersecretstores/externalsecrets/pushsecrets, sealedsecrets, prometheusrules/servicemonitors). The extras should
  REUSE these, not redeploy them (they don't — they add a ClusterSecretStore / rules on top).
- **cert-manager:** managed ships `custom-ca`; LLZ's `openbao-bootstrap-ca` is a different name → coexists. (Only the
  letsencrypt/DNS-01 issuer wiring is apl-core's job on managed — see GAP 2.)
- **Domain discovery:** `otomi/otomi-api` CM's `SSO_ISSUER` present (`https://keycloak.lke<id>.akamai-apl.net/realms/otomi`).

**GAP 1 — block-storage-retain StorageClass (FIXED + validated):** the extras (OpenBao raft PVC) reference
`block-storage-retain` by name, but managed's default is `linode-block-storage-retain` and `block-storage-retain` is
absent (the self-install created it at bootstrap step 5, which the managed path skips). Fix: `bootstrapClusterManaged`
applies LLZ's class (same Linode-CSI provisioner, encryption + Retain).
**[Corrected after this draft — see Corrections at top: it is applied AS the cluster DEFAULT (not the non-default class
described here); the sc-demote pass keeps LKE's `linode-block-storage-retain` non-default so there is a single
encrypted+Retain default, which also lands new app PVCs on encrypted storage. `managedBlockStorageClassYAML` was dropped.]**

**GAP 2 — managed deploys a MINIMAL apl-core app set → the LLZ component subset must be managed-aware (design, not yet
implemented):** managed apl-core deployed only the core (keycloak, otomi, argocd, cert-manager, ESO, sealed-secrets,
gitea, istio, prometheus-operator, cnpg). **Harbor, loki, grafana, tempo, velero, falco, trivy, argo-workflows/events
are ABSENT** (opt-in via the console). So the managed LLZ extras split into three tiers:
1. **Always-applicable** (independent of apl-core optional apps): `openbao`, `externalSecrets` (ClusterSecretStore →
   OpenBao), `reconciler`, `broadPatRotator`, `team-creds` (the team itself is created via the managed console/apl-api,
   which provisions the `team-<name>` keycloak group the OpenBao role binds on — an operational step, not a code gap).
2. **Conditional** on the operator enabling the apl-core app via the console: `harbor` robot-provisioner (needs harbor;
   the provisioner's new systeminfo discovery already retries gracefully when harbor is absent), `observability`
   rules/ExternalSecrets (need loki/grafana/prometheus).
3. **Skip — apl-core owns on managed:** `clusterFoundation` (coredns-custom / **sc-default-patcher** which would DEMOTE
   managed's own default SC / namespaces / network-policies), `certManager` letsencrypt+DNS-01 wiring (keep only
   `openbao-bootstrap-ca`), possibly `argoWorkflows`/`argoEvents`.

   **IMPLEMENTED (build-validated):** the managed-aware component filter is wired into `llz render`.
   - `clusterspec.Bootstrap.ManagedApps []string` (spec field) declares the operator-enabled optional apl-core apps;
     the tier-2 CONDITIONAL components gate on it (render can't discover them — no cluster access).
   - `clusterspec.EmitOnManaged(component, bootstrap)` + the `managedComponentTiers` table encode the 3 tiers
     (drift-guarded). `RenderManifestKustomization` + `committedTargets` take the env `Bootstrap`: on managed they
     select the trimmed base variant, drop tier-3 components, gate tier-2 on `managedApps`, and suppress the
     letsencrypt ACME patch.
   - **certManager split** into `certManagerBootstrapCA` (always — OpenBao's `issuerRef` hard-requires the CA) +
     `certAutomation` (skip — apl-core owns letsencrypt/DNS + the public cert). Repoints openbao's `DependsOn` + the
     cert-manager import mappings.
   - **Managed base variant** `platform-apl/manifest-managed/`: keeps only the AppProjects + the wave-health
     admission guard; excludes cluster-foundation (its sc-default-patcher would demote managed's default SC), the
     apl-core-gap Kyverno policies, the letsencrypt DNS-01 issuers, and the grafana/otel generated-secrets. (The two
     AppProject files are copied local — kustomize's load restrictor blocks sibling-FILE refs — with a drift-guard test.)
     **[Corrected after this draft — see Corrections at top: there is NO separate `manifest-managed/` variant. The
     single `platform-apl/manifest` base was collapsed to drop the self-install-only pieces (cluster-foundation, dns/
     letsencrypt, generated-secrets, apl-core-gap Kyverno policies), keeping the AppProjects + wave-health guard;
     the managed disposition lives on the component registry (`ManagedSkip`/`ManagedConditionalOn`/`EmitOnManaged`).]**

   **Validation:** unit tests (managed render output + drift guards); end-to-end `llz render` of a managed instance
   emits exactly the managed set (base=manifest-managed; carved externalsecrets/harbor/reconciler with harbor gated on
   `managedApps: [harbor]`, observability dropped; plain certManagerBootstrapCA/openbao; no certAutomation/argoWorkflows/
   argoEvents/gitea/policyEngine/imageScanning); `kubectl kustomize` of the rendered overlay produces valid resources
   (AppProjects + wave-health admission + OpenBao CA chain + carved extras) with NO cluster-foundation / letsencrypt /
   cert-automation. Live convergence (ArgoCD syncing a rendered managed instance) is the final gate.

   **KNOWN-INCOMPLETE — managed observability (`managedApps: [loki]`):** the `observability` tier-2 component is wired
   (it emits on managed when `loki` is declared), but the `generated-secrets/` it pairs with (grafana-admin, otel
   ingress bearer) are NOT carried on managed — the managed base variant excludes them and they have not yet moved into
   the observability component. So declaring `loki` today emits the observability extras (loki S3 ExternalSecret, otel
   collector, prometheus rules) while those two self-generated secrets are absent; on managed the otel bearer is
   optional (commented out) and grafana-admin is apl-core's, so this is likely harmless, but it is unproven. Treat
   managed observability as a follow-up: either move `generated-secrets/` into the observability component (finish it)
   or gate its emission until they move. `managedApps: [harbor]` (the fully-built path) is unaffected.

   **Live convergence is a BLOCKING follow-up, not a nice-to-have.** Everything above is unit- + render- + apply-
   validated ("renders and applies cleanly, coexists with apl-core"), but a pushed rendered managed instance reaching
   *Healthy* end-to-end (OpenBao unseals, ESO/reconciler converge) is unproven. Round-1 review found two DETERMINISTIC
   failures (the flag-not-passed + namespace-not-created HIGHs) exactly where live validation had been deferred, so this
   gate must clear before any customer use.

**Node sizing (ADR open Q5):** 3× g8-dedicated-8-4 (~6Gi allocatable/node) hosts the minimal managed apl-core with
some headroom; adding the stateful extras (OpenBao) + any enabled optional apps may want a 4th node or larger type —
size per the enabled app set.
