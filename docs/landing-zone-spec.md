# The LandingZone spec

The LandingZone spec is the declarative front-end for an LKE landing-zone
instance. It is authored as a **split layout**:

- **`landingzone.yaml`** (`kind: LandingZone`) — the **instance identity** (org,
  repo, forge, pinned template version — previously `.copier-answers.yml`) plus
  optional **shared `spec.defaults`** inherited by every deployment.
- **`environments/<env>.yaml`** (`kind: ClusterDefinition`, `metadata.name == env`) —
  one per deployment, holding that cluster's definition + enabled "components"
  (previously the three per-env tfvars and the `apl-values/<env>` manifest
  selection).

The `llz` CLI assembles them into one in-memory resource and reconciles it into
the files the rest of the toolchain already consumes:

| Source of truth | Renders to | When |
|---|---|---|
| `environments/<env>.yaml` → `spec.cluster` | the three `<env>.tfvars` (**gitignored**, regenerated) | build/CI, before `terraform` |
| `environments/<env>.yaml` → `spec.components` | `apl-values/<env>/manifest/kustomization.yaml` (llz Argo backend) + `apl-values/<env>/apl-overlay/apps.yaml` (apl-core backend) — both committed, CI-verified | `llz render` |
| `landingzone.yaml` → `spec.instance` | `.copier-answers.yml` + copier `-d` data | `llz new` / `llz upgrade` |

The per-env `<env>.tfvars` are **build artifacts, not committed** — `terraform-iac-bootstrap/.gitignore`
ignores them, and the `terraform-init` composite action runs `llz render --tfvars-only`
before every terraform op, so the spec is the single source of truth and a spec edit is one
reviewable diff instead of two. **A working `llz` is therefore a hard prerequisite for any
terraform op.** Break-glass: if the renderer is ever broken, run `llz render --tfvars-only`
yourself, or temporarily un-ignore the files and commit them. (The committed `apl-values/<env>/`
manifests stay committed — Argo syncs them from git — and `llz render --check` drift-guards
*those*, not the tfvars.)

This is the **CRD-faithful** shape — one `LandingZone` object plus one
`ClusterDefinition` per env — so graduating to a real CRD + controller later is a
near-mechanical lift, and it gives per-env diff/review locality, per-env
`CODEOWNERS`, and a blast radius of one. Today it is CLI-rendered: `llz render`
reads it, `llz render --check` validates it, and `llz env list` discovers
deployments from it (unioned with any committed `cluster/*.tfvars`).

> **Every new instance has a spec, and it is the only supported authoring path** —
> `llz env add` writes `landingzone.yaml` + `environments/<env>.yaml` on the first
> env, and the per-env tfvars are gitignored artifacts rendered from it (see
> [adopter-guide §3](adopter-guide.md#3-the-values-contract-what-you-must-set)).
> The no-op contract below exists only for **pre-spec instances** scaffolded before
> the spec landed: they keep using their committed tfvars + manifest trees
> unchanged, because every spec-driven path is a no-op when no spec is present.

<!-- toc -->
## Contents

- [Layout](#layout)
- [Full example](#full-example)
- [OpenBao HA pair](#openbao-ha-pair)
- [Minimal example](#minimal-example)
- [Field reference](#field-reference)
- [Commands](#commands)

<!-- /toc -->

## Layout

```
landingzone.yaml          # instance identity + shared defaults
environments/
  prod.yaml               # one ClusterDefinition per deployment …
  staging.yaml            # … metadata.name is the deployment name
```

Deployments live **only** in `environments/<env>.yaml` — authoring `spec.environments`
inline in `landingzone.yaml` is rejected, so there is exactly one place an env is
defined. A `ClusterDefinition`'s `spec` is a cluster definition + its component
toggles; each inherits `landingzone.yaml`'s `spec.defaults`.

**Inheritance precedence:** a per-env value **>** `spec.defaults` **>** the built-in
`terraform.tfvars.example` default. Inheritance is field-level and honors deliberate
zeros — an env's explicit `apiServerAllowCIDRs.ipv4: []` or
`nodePool.autoscalerEnabled: false` overrides a non-empty/true default, while an
omitted field inherits.

## Full example

```yaml
# landingzone.yaml ───────────────────────────────────────────────────────────
# Instance identity (one per repo; was .copier-answers.yml) + shared defaults.
# llz feeds spec.instance to copier as -d data; editing it takes effect on the
# next `llz upgrade`. .copier-answers.yml stays as copier's derived merge record.
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata:
  name: platform-support                  # instance name (repo short name)
spec:
  instance:
    upstreamOrg: akamai-consulting         # → copier upstream_org (template source org)
    repo: my-org/platform-support          # → instance_repo (<owner>/<name>)
    forge: github                          # → forge_flavor (github | github-enterprise | gitlab)
    objLabelPrefix: platform-support       # namespaces this instance's Object Storage
                                           # bucket + key labels (see below)
                                           # (the template pin is NOT here — it lives in
                                           #  .copier-answers.yml, see below)
  defaults:                                # inherited by every ClusterDefinition
    cluster:
      k8sVersion: v1.34.6+lke2             # → k8s_version. SEEDED by `llz env add` from your
                                           # account's catalog (the newest it offers); this
                                           # literal is just an example. Per-account and it
                                           # rotates — `llz ci assert-k8s-version` re-checks it
      nodePool: { type: g8-dedicated-8-4, count: 5 }
      controlPlane: { highAvailability: true, auditLogsEnabled: true }
      # MANDATORY and validated: LLZ never self-installs apl-core. `llz env add`
      # seeds this for you; it is required on every env (here, once, via defaults).
      bootstrap: { managedAppPlatform: true }
```

```yaml
# environments/prod.yaml ──────────────────────────────────────────────────────────
# A standalone production cluster. metadata.name IS the deployment name.
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: prod }
spec:
  cluster:
    clusterLabel: platform-prod                   # → cluster_label
    region: us-ord                                # → region
    # k8sVersion / nodePool / controlPlane inherited from spec.defaults
    tags: [platform, observability, prod]         # → tags
    apiServerAllowCIDRs:
      ipv4: ["203.0.113.0/24"]                    # → github_runner_ipv4_cidrs
      ipv6: []                                    # → github_runner_ipv6_cidrs
    promotionRank: 3                              # → promotion_rank (pipeline position)
    bootstrap:                                    # → apl-core wiring (NOT tfvars — see the
                                                  #   values.yaml note under "Field reference")
      name: platform-prod                         # apl-core cluster.name
      # NO domainSuffix — Linode owns lke<id>.akamai-apl.net and LLZ discovers it
      # in-cluster. Setting it is a hard validation error.
      # aplChartVersion: v6.2.1                   # optional; OMIT IT. An omitted pin tracks the llz
                                                  # baseline, and `llz upgrade` removes a pin llz
                                                  # itself set so it keeps tracking. Linode owns the
                                                  # deployed version on managed App Platform, so this
                                                  # only pins what `llz ci assert-apl-version`
                                                  # resolves — unless manageAplVersion is set,
                                                  # which makes it what the platform deploys.
      aplValues:
        repoURL: https://github.com/my-org/platform-support.git  # apl-core otomi.git.repoUrl
        # revision omitted → the apl-core-owned `apl-prod` branch. It must NOT equal
        # appsRepoRevision — sharing one branch reproduces the converge wedge.
      appsRepoRevision: main                      # apps repo revision
    objectStorage:                                # → object-storage/<env>.tfvars
      cluster: us-ord-7                           # → obj_cluster
      # keyRotationDays: DEPRECATED/ignored — rotation is owned by the
      # in-cluster linodeCredRotator CronJob (obj_key_rotation_days was removed).
  # components omitted → all default-enabled except gitea, cidrFirewall,
  # broadPatRotator, clusterHealthWorkflow (see "Component defaults")
```

```yaml
# environments/staging.yaml ───────────────────────────────────────────────────────
# Earlier in the promotion pipeline; overrides node count + drops Harbor.
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: staging }
spec:
  cluster:
    clusterLabel: platform-staging
    region: us-sea
    nodePool: { count: 3 }                        # override count; type inherited from defaults
    promotionRank: 2
    bootstrap:
      name: platform-staging
      # domainSuffix is never set (Linode owns the domain)
    objectStorage: { cluster: us-sea-1 }
  components:                                        # partial block: only these change
    harbor: { enabled: false }                    # ← no registry in staging
```

## OpenBao HA pair

The HA topology is `ha.role` + `ha.group` on each env; the validator enforces
exactly one `active` and one `standby` per group (across the whole `environments/`
set). A pair is two clusters in **two regions**, so each gets its own region-local
VPC — give them **non-overlapping `network.subnetCIDR`** (the validator rejects
overlapping CIDRs for HA-group members, treating an unset value as the default
`10.0.0.0/13` so a silent collision is caught):

```yaml
# environments/primary.yaml
spec:
  cluster:
    region: us-ord
    ha: { role: active,  group: prod-pair }       # → ha_role / ha_group
    network: { subnetCIDR: 10.0.0.0/13 }       # → vpc_subnet_cidr (/13 or /14)
# environments/secondary.yaml
spec:
  cluster:
    region: us-sea
    ha: { role: standby, group: prod-pair }
    network: { subnetCIDR: 10.8.0.0/13 }       # non-overlapping with the peer
```

## Minimal example

The smallest valid spec — components default to all-on except `gitea`,
`cidrFirewall`, `broadPatRotator`, and `clusterHealthWorkflow`:

```yaml
# landingzone.yaml
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: lab-instance }
spec:
  instance:
    upstreamOrg: akamai-consulting
    repo: my-org/lab-instance
    forge: github
```

```yaml
# environments/lab.yaml
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: lab }
spec:
  cluster:
    clusterLabel: platform-lab
    region: us-sea
    k8sVersion: v1.34.6+lke2
    nodePool: { type: g8-dedicated-8-4, count: 3 }
    bootstrap: { name: platform-lab, managedAppPlatform: true }
    objectStorage: { cluster: us-sea-1 }
```

## Field reference

**Required:** `landingzone.yaml`'s `spec.instance.{upstreamOrg,repo,forge}`,
and per env (`environments/<env>.yaml` or inherited from `spec.defaults`)
`cluster.{clusterLabel,region,k8sVersion}`, `cluster.nodePool.{type,count}`,
`cluster.bootstrap.name`, and **`cluster.bootstrap.managedAppPlatform: true`**
(LLZ never self-installs apl-core — `llz env add` seeds it into `spec.defaults`).

**`spec.instance.objLabelPrefix`** — the per-instance namespace on every Object
Storage bucket and key label, so a deployment's buckets become
`<objLabelPrefix>-loki-{chunks,ruler,admin}-<env>` and
`<objLabelPrefix>-harbor-registry-<env>`.

Optional in the file, but never optional in effect: when unset it is derived from
`metadata.name` (lowercased, illegal characters collapsed to hyphens), and
`llz env add` writes the derived value out so the effective prefix is visible.
`llz render` emits it as the object-storage root's `label_prefix`, and
`llz render` also threads it to the in-cluster rotator as `OBJ_LABEL_PREFIX` —
that pod has no spec to read.

**Why it exists.** Linode Object Storage bucket labels share **one namespace per
region, across accounts**. A shared prefix means the first instance to use a given
deployment name in a region takes those names globally and every later one fails
its apply with `[400] ... already exists`. Key labels carry the prefix for a
related reason: they are per-account, but `llz reap` and the rotation table match
by exact label, so two instances in one Linode account would otherwise rotate and
delete each other's keys.

**Do not change it after an apply.** A bucket label cannot be renamed in place, and
the module declares no `create_before_destroy`, so Terraform plans
**destroy-then-create** on all four buckets. There is no drain on the apply path
(only on destroy), so if the old buckets still hold objects the apply fails with
`bucket is not empty` and keeps failing until you empty them by hand; if they are
empty, it deletes them and their data with them. Treat it as a rebuild, not a
rename: drain and destroy the old object-storage root first, or keep the value.

**Must NOT be set:** `cluster.bootstrap.domainSuffix` — Linode owns the
`lke<id>.akamai-apl.net` domain and LLZ discovers it in-cluster; a stale value
would misroute the Keycloak issuer and Harbor URL, so the validator rejects it
outright. (The `--cluster-domain` flag on `llz env add` is deprecated and ignored;
passing it now warns and writes nothing, where it used to echo a `domainSuffix`
back in the summary banner as though it had been applied.)

**Deprecated: `spec.instance.templateVersion`.** Accepted and ignored — leave it or
delete it, nothing reads it. The template pin lives once in `.copier-answers.yml`
(`llz_version`), which `llz upgrade` maintains and `llz render` resolves the shared
`platform-apl` refs from. The spec copy was never read and nothing re-stamped it on
upgrade, so it only drifted: one live instance sat at `v0.0.31` while every other pin
in the repo said `v0.0.34`.

**Components — one toggle, two backends.** `spec.components.<name>` is the single
"what's deployed" switch. Each component routes to whichever backend(s) deliver it:
the **llz Argo backend** (its resources/Applications live ONCE in a shared kustomize
Component, `platform-apl/components/<name>/`, which the env's thin
`apl-values/<env>/manifest/kustomization.yaml` lists under `components:` when enabled —
`llz render` generates that overlay and `llz render --check` drift-guards it) and/or the
**apl-core backend** (it flips `apps.<key>.enabled` in the committed
`apl-values/<env>/apl-overlay/apps.yaml`, which the in-cluster apl-overlay reconciler
merges onto the `apl-<env>` branch — `llz render` generates it and `--check`
drift-guards it). Some span both — e.g. `harbor`
enables apl-core's Harbor app *and* adds the llz registry-S3 ExternalSecret;
`observability` enables apl-core's prometheus/loki/grafana/alertmanager/otel *and*
adds the loki ExternalSecret + alert rules.

Omit the `components:` block and every component is enabled except `gitea`,
`cidrFirewall`, `broadPatRotator`, and `clusterHealthWorkflow`. A partial
block changes only the components you name — an explicit `enabled: false` sticks;
unmentioned components default on. `enabled` is tri-state: omitting it (a tune-only
toggle, see below) inherits the default rather than reading as a disable. The set:
`argocd` (mandatory), `clusterFoundation` (mandatory), `externalSecrets`,
`certManagerBootstrapCA`, `imageSignature`,
`openbao` (requires `externalSecrets` + `certManagerBootstrapCA`),
`argoWorkflows`, `argoEvents`, `observability`, `harbor`,
`policyEngine` (Kyverno + policy-reporter), `imageScanning` (Trivy), `gitea`,
`cidrFirewall`, `broadPatRotator` (requires `externalSecrets`),
`llzReconciler` (requires `observability`), `clusterHealthWorkflow`
(requires `argoWorkflows`). `llz components` prints this table live — it is the
authoritative copy.
On the managed platform many of these are apl-core's (Linode-owned) and are not
emitted by `llz render` — see docs/adr/0005-managed-app-platform.md.

For three of them the toggle is **inert**, not merely unemitted: `gitea`,
`policyEngine` and `imageScanning` are `ManagedSkip`, so LLZ ships neither
manifests nor an apps-overlay toggle for them and setting `enabled` in the spec
changes nothing on any cluster LLZ builds. They are enabled in the App Platform
Console, where apl-core owns them. The registry keeps the entries so the apl-core
surface LLZ deliberately does not drive is still described.

> **`argoWorkflows` on managed is opt-in even though it is default-on elsewhere.**
> It is a support component: on managed its only platform consumer is
> `clusterHealthWorkflow`, so it emits when that is enabled — otherwise every managed
> cluster would carry a workflow controller it never runs. If you want the Workflow
> CRDs for **your own** workloads (a build pipeline under `kubernetes-custom/`, say),
> name it directly — `argoWorkflows: { enabled: true }` — rather than switching on
> `clusterHealthWorkflow` for its dependency side effect. Without either, your
> `WorkflowTemplate` fails to sync with `WorkflowTemplate.argoproj.io "" not found`,
> which names the CRD that is missing but not the reason.
(There is no `volumeLabeler` — it became the `volume-labels` lane of
`llz reconcile`, which is itself now retired — and no `dns` component. `Validate` rejects unknown keys, so
naming either is a hard spec error.)

**Per-component sizing.** A few components take capacity knobs alongside `enabled`.

> ⚠️ **These four knobs currently reach no cluster.** They are parsed, merged,
> validated and shown by `llz apl app list`, but nothing renders them: their only
> render target was the per-env `apl-values/<env>/values.yaml`, which `llz render`
> stopped emitting when LLZ moved to the managed App Platform (ADR
> [0005](adr/0005-managed-app-platform.md)) — apl-core owns its own values there.
> Setting them changes the spec and nothing else; the cluster runs apl-core's
> defaults. `llz doctor` reports it when an instance sets one. Wiring them means
> carrying them through the apl-overlay's per-app channel
> (`apl-values/_shared/apl-overlay/appvalues.yaml`), which needs apl-core's AplApp
> schema for these first-class keys confirmed against a live cluster first — see
> [upstream-asks.md](upstream-asks.md).

`observability` takes `retention` (→
`apps.prometheus.retention`, default `7d`), `storage` (→ `storageSize`, default
`10Gi`), and `replicas` (default `1`); `harbor` takes `registryStorage` (registry
image-store PVC, default `20Gi`); `broadPatRotator` takes `broadPATLabel` +
`broadPATDeployments` (which PAT it rotates and where it propagates — not capacity).
Those three are the whole set. An unset knob keeps the base default; a knob set
on a component that doesn't read it (or a bad duration/quantity) is a validation
error. Example: `observability: { retention: 30d, storage: 50Gi, replicas: 2 }`.

**Optional fields are only written when set.** The optional bools
(`nodePool.autoscalerEnabled`, `controlPlane.highAvailability`,
`controlPlane.auditLogsEnabled`), the optional pool bounds
(`nodePool.autoscalerMin` / `nodePool.autoscalerMax`, defaulting to 3 and 6) and
the `apiServerAllowCIDRs` lists are written to the tfvars only when you specify
them (on the env or in `spec.defaults`); omit them and the
`terraform.tfvars.example` default is left untouched.

**Shared defaults.** `spec.defaults.cluster` / `spec.defaults.components` in
`landingzone.yaml` set a baseline inherited by every environment; a per-env value
overrides it field-by-field (see [Layout](#layout)).

**Team-scoped OpenBao writes (`spec.teams`, instance-wide).** Each entry —
`{name, openbaoSubtree}` — gives a group of human operators scoped, **non-root**
WRITE access to OpenBao: `llz render` emits a `teamConfig.<name>` into the
apl-values overlay (apl-core provisions the native team — a namespace + the
Keycloak realm group/role `team-<name>`), `llz ci bao-configure` writes a
`<name>-writer` policy (`create/update/read` on `<openbaoSubtree>/*`) + a
`keycloak` OIDC role bound on the `groups` claim value `team-<name>`, and
operators mint a short-lived token with `llz openbao login --team <name>`. So the
subtree is also **consumable** in-cluster, `bao-configure` additionally writes a
read-only `<name>-reader` policy and attaches it to ESO's `eso` Kubernetes-auth
role — an `ExternalSecret` pointing at `<openbaoSubtree>/*` resolves through the
`openbao` ClusterSecretStore with no extra wiring. That store is namespace-
unconditioned, so the read grant is **not** a per-team confidentiality boundary
(see [the runbook](runbooks/openbao-team-login.md)).
`name` is lowercase kebab and **may not** be `admin`, `platform-admin`, or
`all-teams-admin` (apl-core owns those); `openbaoSubtree` must be a plain path
prefix under `secret/` (no glob, no trailing `/`) and **may not** sit inside a
platform-owned namespace (`secret/{linode,harbor,grafana,loki,otel,alerts,infra,cert-automation}`),
so a team can't grant itself write on a system credential like the Linode PAT. **New clusters** get one team
scaffolded automatically: `llz new` writes a `spec.teams` entry from the
`openbao_team` question (default **`platform`** → `secret/platform`), so a
non-root write path exists out of the box. **Existing clusters** are left
untouched — there is no load-time default — and opt in by adding a team here (the
retrofit path). Full walkthrough:
[docs/runbooks/openbao-team-login.md](runbooks/openbao-team-login.md).

> **`llz render` does not write apl-core's `values.yaml`.** LLZ runs exclusively on
> Linode's **managed** App Platform, where apl-core owns its own values (ADR
> [0005](adr/0005-managed-app-platform.md)) — `template-scripts/ci/scaffold-render-check.sh`
> *fails the build* if a render ever emits one. The spec's route into apl-core is the **apl-overlay**
> (`apl-values/<env>/apl-overlay/{apps,obj,teams}.yaml`) — a separate, secret-free
> source of truth the in-cluster reconciler merges onto the `apl-<env>` branch, so
> the two writers never collide.
>
> **Consequence — three spec blocks are currently validated but never rendered:**
> `spec.dns.*` (including `acmeEmail`), `spec.defaults.platform.*` (`externalDNS`,
> `externalIDP`), and `spec.alerting.*` (`receivers`, `slack.channel*`). They were
> written into `values.yaml` before the managed-only collapse and no renderer
> replaced them, so setting them today changes `landingzone.yaml` and nothing else.
> `llz import` still reads them back off a live site. Treat them as inert until
> they are rewired or removed.

Cluster identity does still reach the platform, just not through `values.yaml`:
`cluster.bootstrap.domainSuffix` drives the Harbor host patch
(`llz render` → `harbor-provisioner-env-patch.yaml`) and `llz apl user add`'s
portal URL.

**Networking.** A Linode VPC is a **region-scoped container** (it has no CIDR —
subnets do). By default each environment gets its **own dedicated VPC**
(`<cluster_label>-vpc`); `cluster.network.subnetCIDR` (→ `vpc_subnet_cidr`, a `/13`
or `/14`) sets that VPC's single worker subnet.

To put several **same-region** environments in **one** VPC, declare it under
`landingzone.yaml`'s `spec.networks` (name → region) and reference it per env with
`cluster.network.vpc`; each env then carves its own subnet:

```yaml
# landingzone.yaml
spec:
  networks:
    ord-shared: { region: us-ord }
# environments/web.yaml → network: { vpc: ord-shared, subnetCIDR: 10.0.0.0/14 }
# environments/api.yaml → network: { vpc: ord-shared, subnetCIDR: 10.4.0.0/14 }
```

The validator enforces: a referenced network exists and is in the **same region**
as the env (VPCs can't span regions); **subnets sharing a VPC don't overlap**
(Linode rejects overlapping subnets in a VPC); and **HA-group members** (always
different regions/VPCs) use distinct CIDRs as peering hygiene. Unset CIDRs resolve
to the `10.0.0.0/13` default for the overlap check, so a silent collision is caught.

**Blast radius — keep prod in its own network.** Each shared VPC is its own
Terraform state (`vpc/<network>`), so a change to one **cannot** touch another —
different networks are fully isolated. The danger is *mixing tiers in one VPC*: if
a non-prod env shares prod's network, a non-prod build's VPC apply runs against
prod's state. So **never share a VPC across the prod / non-prod boundary**, and —
since a VPC is region-scoped — name networks **per region** (`prod-ord`, `prod-sea`,
`nonprod-ord`, …). The scaffold's
[`landingzone.yaml.example`](../instance-template/landingzone.yaml.example) shows
this. Note that a **multi-region prod HA pair with one cluster per region needs no
shared network at all** — each cluster is alone in its region, so a dedicated VPC is
both correct and the most isolated; reach for a shared network only to co-locate
several clusters in one region.

> **Shared-VPC apply: built; one live check remains.** Schema, validation, render,
> the per-network `vpc` root (state `vpc/<network>`), the `llz-cluster` `vpc_id`
> attach, the cluster root's label lookup, and the `apply-vpc` workflow job
> (per-network, serialized by a concurrency group, runs before `apply-cluster`)
> are all in place. What remains is a real `plan`/`apply` against Linode to confirm
> the `data.linode_vpcs` lookup + attach end-to-end. The **dedicated-VPC default is
> unaffected** and fully supported.

**Injected automatically.** `deployment`, `apl_values_env`, and `region_suffix`
are always set to the env key, so they can never drift out of sync.

## Commands

```sh
llz render               # render every environment's tfvars from the spec
llz render staging       # render just one environment
llz render --check       # validate the spec; write nothing (used as a CI guard)
llz env list             # deployments from the spec ∪ committed cluster/*.tfvars
```

`llz render --check` reports every problem at once — unknown component names, a
disabled mandatory component, `openbao` missing its dependencies, an HA group that
is not a clean active/standby pair, an invalid `forge`, `spec.environments`
authored inline in `landingzone.yaml`, and so on.
