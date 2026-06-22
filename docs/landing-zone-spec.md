# The `llz.yaml` LandingZone spec

`llz.yaml` is the single declarative front-end for an LKE landing-zone instance.
One repo-root file holds the **instance identity** (org, repo, forge, pinned
template version — previously `.copier-answers.yml`) **and every deployment's
cluster definition + enabled "recipes"** (previously the three per-env tfvars and
the `apl-values/<env>` manifest selection). The `llz` CLI reconciles it into the
files the rest of the toolchain already consumes:

| Source of truth in `llz.yaml` | Renders to | When |
|---|---|---|
| `spec.environments.<env>.cluster` | the three `<env>.tfvars` (transient, working-tree) | build/CI, before `terraform` |
| `spec.environments.<env>.recipes` | `manifest/kustomization.yaml` + `argocd/kustomization.yaml` (committed, CI-verified) | `llz render` |
| `spec.instance` | `.copier-answers.yml` + copier `-d` data | `llz new` / `llz upgrade` |

The resource uses the Kubernetes `apiVersion/kind/metadata/spec` shape so it can
graduate to a real CRD + controller later without a rewrite. Today it is
CLI-rendered: `llz render` reads it, `llz render --check` validates it, and
`llz env list` discovers deployments from it (unioned with any committed
`cluster/*.tfvars`, so an instance can migrate env-by-env).

> Adopting the spec is opt-in. Instances without one keep using their committed
> tfvars + manifest trees unchanged; every spec-driven path is a no-op when no
> spec file is present.

## Two layouts: single file vs. split

The spec is authored in one of two on-disk shapes. Both assemble into the **same**
in-memory model, so `llz render`, the tfvars mapping, and every validator behave
identically — pick by instance size.

| | **Single file** (simple mode) | **Split** (the fleet default) |
|---|---|---|
| Files | one `llz.yaml` | `landingzone.yaml` + `clusters/<env>.yaml` |
| Per-env kind | inline `spec.environments.<env>` | `kind: ClusterDefinition`, `metadata.name == env` |
| Best for | a 1–2 env instance | several prod/staging/lab/HA-pair envs |
| Wins | whole instance in one diff | per-env diffs, per-env `CODEOWNERS`, blast radius of one |
| Shared defaults | `spec.defaults` (optional) | `spec.defaults` in `landingzone.yaml` |

The split layout is the **CRD-faithful** shape — one `LandingZone` object plus one
`ClusterDefinition` per env — so graduating to a controller is near-mechanical. A
`clusters/<env>.yaml`'s `spec` is exactly a single-file `spec.environments.<env>` —
the same fields, just relocated.

```yaml
# landingzone.yaml — instance identity + shared defaults
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: platform-support }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: my-org/platform-support, forge: github, templateVersion: v0.4.0 }
  defaults:                                    # inherited by every ClusterDefinition
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 5 }
      controlPlane: { highAvailability: true, auditLogsEnabled: true }
---
# clusters/prod.yaml — one per env; metadata.name IS the deployment name
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: prod }
spec:
  cluster:
    clusterLabel: platform-prod
    region: us-ord                             # k8sVersion / nodePool inherited from defaults
    bootstrap: { name: platform-prod }
    objectStorage: { cluster: us-ord-1 }
```

**Inheritance precedence:** a per-env value **>** `spec.defaults` **>** the built-in
`terraform.tfvars.example` default. Inheritance is field-level and honors deliberate
zeros — an env's explicit `apiServerAllowCIDRs.ipv4: []` or
`nodePool.autoscalerEnabled: false` overrides a non-empty/true default, while an
omitted field inherits. `spec.defaults` also works in the single-file layout.

> **One layout per instance.** Keep either `llz.yaml` **or** `landingzone.yaml`,
> not both — `llz` errors on an ambiguous mix.

## Full example

The single-file layout, with every field shown inline:

```yaml
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata:
  name: platform-support           # instance name (repo short name)

spec:
  # ── Instance identity (one per repo; was .copier-answers.yml) ──────────────
  # llz feeds these to copier as -d data; editing them takes effect on the next
  # `llz upgrade`. .copier-answers.yml stays as copier's derived merge record.
  instance:
    upstreamOrg: akamai-consulting        # → copier upstream_org (template source org)
    repo: my-org/platform-support         # → instance_repo (<owner>/<name>)
    forge: github                         # → forge_flavor (github | github-enterprise | gitlab)
    templateVersion: v0.4.0               # → llz_version (pinned release, or "main")

  # ── Deployments (was cluster/*.tfvars + the apl-values/<env> overlay) ──────
  environments:

    # ---- a standalone production cluster ----
    prod:
      cluster:
        clusterLabel: platform-prod                 # → cluster_label
        region: us-ord                              # → region
        k8sVersion: v1.33.6+lke7                    # → k8s_version
        tags: [platform, observability, prod]       # → tags
        nodePool:
          type: g8-dedicated-8-4                    # → node_type
          count: 5                                  # → node_count
          autoscalerEnabled: false                  # → autoscaler_enabled
        controlPlane:
          highAvailability: true                    # → control_plane_high_availability
          auditLogsEnabled: true                    # → control_plane_audit_logs_enabled
        apiServerAllowCIDRs:
          ipv4: ["203.0.113.0/24"]                  # → github_runner_ipv4_cidrs
          ipv6: []                                  # → github_runner_ipv6_cidrs
        promotionRank: 3                             # → promotion_rank (pipeline position)
        bootstrap:                                   # → cluster-bootstrap/<env>.tfvars
          name: platform-prod                        # → cluster_name
          domainSuffix: prod.example.com             # → cluster_domain
          aplChartVersion: 5.0.0                     # → apl_chart_version
          aplValues:
            repoURL: https://github.com/my-org/platform-support.git  # → apl_values_repo_url
            revision: main                           # → apl_values_repo_revision
          appsRepoRevision: main                     # → apps_repo_revision
        objectStorage:                               # → object-storage/<env>.tfvars
          cluster: us-ord-7                          # → obj_cluster
          keyRotationDays: 90                        # → obj_key_rotation_days (≤120)
      # recipes omitted → all default-enabled except dns (see "Recipe defaults")

    # ---- a staging cluster, earlier in the promotion pipeline ----
    staging:
      cluster:
        clusterLabel: platform-staging
        region: us-sea
        k8sVersion: v1.33.6+lke7
        nodePool: { type: g8-dedicated-8-4, count: 3 }
        promotionRank: 2
        bootstrap:
          name: platform-staging
          # domainSuffix omitted → defaults to "staging.internal"
        objectStorage: { cluster: us-sea-1 }
      recipes:                                       # explicit toggles for this env
        clusterFoundation:  { enabled: true }        # mandatory (wave -20)
        argocd:             { enabled: true }        # mandatory
        externalSecrets:    { enabled: true }
        certManager:        { enabled: true }
        openbao:            { enabled: true }        # requires externalSecrets + certManager
        argoWorkflows:      { enabled: true }
        argoEvents:         { enabled: true }
        volumeLabeler:      { enabled: true }
        observability:      { enabled: true }
        harbor:             { enabled: false }       # ← no registry in staging
        dns:                { enabled: false }       # applied separately by bootstrap-dns.yml
```

## OpenBao HA pair

The HA topology is `ha.role` + `ha.group` on each env; the validator enforces
exactly one `active` and one `standby` per group:

```yaml
  environments:
    primary:
      cluster:
        # ...
        ha: { role: active,  group: prod-pair }     # → ha_role / ha_group
    secondary:
      cluster:
        # ...
        ha: { role: standby, group: prod-pair }
```

## Minimal example

The smallest valid spec — recipes default to all-on except `dns`, and
`domainSuffix` defaults to `<env>.internal`:

```yaml
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata:
  name: lab-instance
spec:
  instance:
    upstreamOrg: akamai-consulting
    repo: my-org/lab-instance
    forge: github
    templateVersion: main
  environments:
    lab:
      cluster:
        clusterLabel: platform-lab
        region: us-sea
        k8sVersion: v1.33.6+lke7
        nodePool: { type: g8-dedicated-8-4, count: 3 }
        bootstrap: { name: platform-lab }
        objectStorage: { cluster: us-sea-1 }
```

## Field reference

**Required:** `spec.instance.{upstreamOrg,repo,forge,templateVersion}`, and per
env `cluster.{clusterLabel,region,k8sVersion}`, `cluster.nodePool.{type,count}`,
`cluster.bootstrap.name`.

**Recipe defaults.** Omit the `recipes:` block entirely and every recipe is
enabled except `dns`. Provide a partial block and only the recipes you name are
changed — an explicit `enabled: false` sticks, and unmentioned recipes still
default on. The recipe set: `clusterFoundation` (mandatory), `argocd`
(mandatory), `externalSecrets`, `certManager`, `openbao` (requires
`externalSecrets` + `certManager`), `argoWorkflows`, `argoEvents`,
`volumeLabeler`, `observability`, `harbor`, `dns` (default off — applied
separately by `bootstrap-dns.yml`).

**Optional fields are only written when set.** The optional bools
(`nodePool.autoscalerEnabled`, `controlPlane.highAvailability`,
`controlPlane.auditLogsEnabled`) and the `apiServerAllowCIDRs` lists are written
to the tfvars only when you specify them; omit them and the
`terraform.tfvars.example` default is left untouched.

**Shared defaults.** `spec.defaults.cluster` / `spec.defaults.recipes` set a baseline
inherited by every environment; a per-env value overrides it field-by-field (see
[Two layouts](#two-layouts-single-file-vs-split)). Common in the split layout, but
valid in either.

**Injected automatically.** `deployment`, `apl_values_env`, and `region_suffix`
are always set to the env key, so they can never drift out of sync.

## Commands

```sh
llz render               # render every environment's tfvars from the spec
llz render staging       # render just one environment
llz render --check       # validate the spec; write nothing (used as a CI guard)
llz env list             # deployments from the spec ∪ committed cluster/*.tfvars
```

These work the same against either layout — `llz` auto-detects `llz.yaml` vs.
`landingzone.yaml` + `clusters/*.yaml` and assembles the same model before rendering.

`llz render --check` reports every problem at once — unknown recipe names, a
disabled mandatory recipe, `openbao` missing its dependencies, an HA group that
is not a clean active/standby pair, an invalid `forge`, and so on.
