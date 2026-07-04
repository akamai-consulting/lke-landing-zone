# Design: apl-core 5.x → 6.x migration

**Status:** In progress — code landed on branch `feat/apl-core-v6-migration`;
pinned to the GA `v6.0.0` release (published 2026-07-01). Validate in lab before
any non-lab promotion.
**Relates to:** [apl-core-migration-runbook.md](../apl-core-migration-runbook.md),
[../secrets.md](../secrets.md), [linode-credential-rotator.md](linode-credential-rotator.md),
`instance-template/apl-values/`, `instance-template/terraform-iac-bootstrap/cluster-bootstrap/`,
`tools/cmd/llz/ci_openbao_configure.go`.

## Why

The landing zone pinned the apl-core Helm chart at `5.0.0`; this migration moves
it to the GA `6.0.0` release. apl-core
6.x reworks several platform components the landing zone integrates with. This
design captures what changed, what we did to the **template**, and how to
**upgrade downstream instances** (generated repos + their live clusters).

> **GA status.** `6.0.0` GA'd on 2026-07-01 and is the pin we use. The migration
> work was developed against `v6.0.0-rc.12`; the rc.12→GA delta is limited
> (component version bumps, an operator-image chart-value refactor from
> `imageName`/`otomi.useORCS` to `operator.image.repository`, and the SOPS
> `kms` block / `operator.gitOrg`/`operator.gitRepo` values removed from the
> `apl` chart) — none of which the landing zone overrides, so the bump is a
> straight re-pin. Validate in lab before promoting past lab.

## What changed in v6 (evidence from the `v5.0.0…v6.0.0` source diff)

| Change | Source signal | Impact on the landing zone |
|---|---|---|
| **ESO is now a core, always-on app** | new `values/external-secrets/` + `external-secrets:` schema block; helmfile-01 installs it (gated on `sealed-secrets.enabled`) in the `external-secrets` ns; CRDs shipped statically under `charts/external-secrets/crds/` | The repo installed its own ESO (`llz-external-secrets`) because 5.0.0 shipped none. Two cluster-scoped controllers conflict → **retired the self-managed ESO**, rewired everything to apl-core's. |
| **Gitea default-off; new `git-server`** | `defaults.yaml`: `gitea.enabled:false`, `git-server.enabled:true`; `apl-gitea-operator` now gated on `gitea.enabled` (helmfile-03) | The 5.0.0 "temporary gitea" hack is gone — **disabled gitea**; v6 runs gitops against the external BYO-Git repo. |
| **`apl-sops-secrets` envFrom now `optional:true`** | `charts/apl-operator/templates/deployment.yaml` | **Deleted** the empty-Secret placeholder workaround in cluster-bootstrap. |
| **ingress-nginx removed; Gateway-API only; TLS-passthrough removed** | `ingressNginx` schema block + `values/ingress-nginx` deleted | The repo uses Istio Gateways and has zero ingress-nginx references → low impact. **Verify** Gateway-API CRD provisioning. |
| **Istio ambient mode** (ztunnel + istio-cni) | new `values/ztunnel`, `values/istio-cni`; `ambient:false` default | Default stays sidecar (non-breaking now). Interacts with the PSS-`restricted`-vs-sidecar carve-outs in `llz-cluster-foundation`. **Do not enable ambient** without review. |
| **promtail removed; Loki 6.55** | `values/promtail` deleted | **Re-verify** the Loki-gateway nginx-resolver render (`cluster-bootstrap/main.tf`); the Loki chart was restructured. |
| ServiceEntry resolution improvements (5.1.0) | 5.1.0 notes | May allow shrinking the CoreDNS `*.internal` rewrite workaround. **Verify** in lab. |
| Argo Workflows / Argo Events still NOT bundled | absent from `values/` | No change — keep shipping them. |
| Tekton still present & default | `defaults.yaml` | `tekton.enabled:false` stays valid. |
| New: `team-secrets` (git-based), `recovery` mode, `wildcardDomainOrIp` | schema additions | Optional; no action. |

## What we implemented (template), by commit

1. **Pin bump** → `6.0.0` (tfvars example, spec env example, adopter guide).
2. **Drop `apl-sops-secrets` placeholder** — fixed upstream (`optional:true`).
3. **ESO migration to apl-core's bundled operator.** Deleted the
   `llz-external-secrets-operator` Argo Application; the `externalSecrets`
   component now only gap-fills the NetworkPolicies apl-core does not ship for
   its ESO namespace. Repointed at apl-core's ESO (namespace `external-secrets`,
   controller SA `external-secrets`):
   - OpenBao k8s-auth roles `eso`/`eso-pusher` bind `external-secrets/external-secrets`
     (`ci_openbao_configure.go`);
   - the `openbao` / `openbao-push` ClusterSecretStores reference that SA;
   - the grafana/otel generated-secrets moved into the `external-secrets` ns;
   - OpenBao NetworkPolicy allowlist, platform-support AppProject destinations,
     and the `esoNamespace` health constant updated;
   - dropped the now-unused `llz-external-secrets` namespace from
     cluster-foundation; updated the (non-live) bootstrap-apps generator chart.
4. **Disable in-cluster Gitea** (v6 default; `apl-gitea-operator` gated on
   `gitea.enabled`); refreshed the argocd-namespace label rationale and the
   PVC-encryption-policy comments (no more `gitea-valkey` PVC).
5. **Docs / comments** — this design, the runbook pin reference, and the stale
   `apl-core 5.0.0` comments.
6. **Loki admin password → keep supplying it (TF-generated).** ⚠️ **GA
   correction.** The migration first *dropped* `apps.loki.adminPassword` on the
   theory that v6's x-secret generator (`x-secret: '{{ randAlphaNum 20 }}'`)
   self-wires it. **The e2e disproved this:** v6's helm `values.schema.json`
   marks `apps.loki.adminPassword` **required**, and cluster-bootstrap does a raw
   `helm install apl/apl` that validates the schema *before* apl-operator (the
   thing that would run the generator) can install — so the install hard-fails
   `apps.loki: adminPassword is required`. The x-secret generator only runs inside
   otomi's own bootstrap, which we bypass. Fix: supply it again, but
   self-contained — a `random_password.loki_admin` in cluster-bootstrap rendered
   into `apps.loki.adminPassword` via templatefile (stable in TF state, no churn),
   **not** the old `TF_VAR_loki_admin_password`/`LOKI_ADMIN_PASSWORD` GitHub-secret
   path (nothing outside apl-core consumes this value). The `secrets.md` "Known
   limitation" stands (this cred is not on the ESO/OpenBao rotation lifecycle).

### GA follow-ups (v6.0.0 source review, 2026-07-03)

Three code-level cleanups landed with the GA re-pin:

- **oauth2-proxy `wait-for-keycloak` Kyverno policy rewritten domain-agnostic.**
  The old wholesale args patch hardcoded the legacy `keycloak.primary.internal`
  URL and would have clobbered v6's `keycloak.<domainSuffix>` poll on every
  instance. The policy itself is still needed while `apps.cert-manager.issuer`
  stays at its `custom-ca` default.
- **TF `apl_values_repo_creds` Secret retired.** v6's `argocd-raw.gotmpl`
  self-registers `argocd-repo-creds-*` for `otomi.git.repoUrl` from
  `apl-git-config`, resolving N4-M1.
- **Dead `charts.external-secrets.io` entries pruned** from the AppProject
  `sourceRepos` allowlist (the self-managed ESO chart source is gone).

Two further findings are **lab-gated** — folded into the checklist below:

- [ ] **Narrow (not delete) the CoreDNS `*.internal` rewrite — keycloak MUST stay.**
      *Correction to the first-pass read below.* The rewrite serves
      sidecar-less/init-container resolution of apl-core's `<svc>.<domainSuffix>`
      ServiceEntry hosts; with `domainSuffix = <env>.internal` the `*.internal`
      suffix wildcard catches them all. The earlier "keycloak.internal gone /
      harbor.internal remains" evidence was literal-string grep noise that does
      **not** map to the templated hosts — verified against v6 source:
      - **`keycloak.<domainSuffix>` is still required.** The oauth2-proxy
        `wait-for-keycloak` init container is sidecar-less by definition and polls
        `https://keycloak.<domainSuffix>/realms/otomi` at boot — the *same* init
        container the `-skw` Kyverno policy patches (the rewrite lets it RESOLVE,
        the policy lets curl not fail on the custom-CA cert). v6 added
        `KEYCLOAK_ADDRESS_INTERNAL` (in-cluster svc) for the keycloak OPERATOR's
        backchannel, but NOT for that init container.
      - The `harbor.internal` "13 hits" are Harbor's own chart config
        (`database.type: internal`, `internalTLS.*`), unrelated to the
        `harbor.<domainSuffix>` ServiceEntry the rewrite serves.
      v6 ServiceEntry hosts on this stack: argocd, harbor, keycloak, auth
      (oauth2-proxy), api + tty (otomi-api), console (otomi-console); gitea is
      disabled. Lab task: for each host, check whether any sidecar-less pod
      (`sidecar.istio.io/inject: "false"`) or init container resolves it — keep
      keycloak, drop only hosts with zero such consumers. One-shot inventory:
      `kubectl get pods -A -o json | jq -r '.items[] | select((.spec.initContainers|length>0) or (.metadata.annotations["sidecar.istio.io/inject"]=="false")) | .metadata.namespace+"/"+.metadata.name'`
      then grep those pods' env/args for `.<domainSuffix>` hosts.
- [ ] **Retest `node_count` 4 (optionally 3).** v6 reduces platform resource
      requests by 0.8 CPU / 1 GB RAM / 6 PVs (release notes), and gitea's PVCs
      disappear (gitea disabled on this branch). The cluster/ root's
      `node_count` default of 5 was sized against 5.0.0, where OpenBao
      followers went Pending on Insufficient memory / max Block Storage volume
      count on 3 nodes. Retest node_count 4 (and optionally 3) on a v6 lab
      bootstrap before relaxing the default.

**Decision — apl-core-native object storage (`obj.provider.linode`): evaluated
and declined for now.** v6's `values/loki/loki.gotmpl` and
`values/harbor/harbor.gotmpl` natively wire S3 (`object_store: s3`,
`imageChartStorage` type `s3`) from
`obj.provider.linode.{region,accessKeyId,secretAccessKey,buckets}`. Adopting it
would retire the landing zone's `kyverno-loki-s3-object-store` mutation policy,
the `loki-object-store` + `harbor-registry-s3` ExternalSecrets, and the
`_rawValues` S3 blocks — BUT it requires static S3 credentials in apl-core's
values (sealed, rotated only by values edits), which conflicts with the landing
zone's in-cluster key-rotation model (linode-cred-rotator: keys minted at
bootstrap, rotated without touching git), and apl-core's model is
one-bucket-per-app vs the landing zone's three Loki buckets
(chunks/ruler/admin). **Keep the landing-zone S3 wiring.** Revisit if the
rotation model changes or apl-core learns to source obj creds from a Secret.

### Round-2 simplification review (2026-07-03 source pass)

A second pass looked for further v6-enabled cleanups. Result: **almost nothing
else is removable** — the remaining workarounds are confirmed load-bearing on v6:

- **Loki: all three workarounds stay.** The gateway nginx-resolver TF read stays
  (grafana/loki 6.55.0 still defaults the resolver to a hostname → crashloop; v6
  adds no resolver). The `kyverno-loki-s3-object-store` mutation stays — v6 fixed
  the schema-date round-trip but emits `object_store: s3` only under
  `obj.provider.type: linode`, which we don't set, so the `filesystem→s3` flip is
  still load-bearing. Its removal is **coupled to the declined obj-storage
  decision above** (same migration). The `loki-object-store` ES namespace
  (`monitoring`) is still correct (only Grafana moved namespaces on v6).
- **TF bootstrap timing/pre-creation workarounds: all stay.** v6's `apl` chart
  still ships the annotation-less `00-namespace.yaml` (namespace-adopt collision
  is real), apl-operator still has no readinessProbe (helm wait covers only the
  Deployment), oauth2-proxy's redis-ha PVC still hardcodes `linode-block-storage`,
  and the sc-default-demote race is LKE-Flux (not apl-core).
- **apl-core defaults don't obviate LLZ config.** `platformBackups.*` default off
  (CNPG); grafana/harbor `adminPassword` are `x-secret: ''` (supplied, not
  generated); v6 *removed* the OTLP collector, so LLZ's OTelCollector CR is still
  required.
- **Bug fixed, not a simplification:** the `gitea` component in
  `tools/internal/clusterspec/components.go` lacked `DefaultDisabled`, so
  `llz render` would flip the committed `gitea: { enabled: false }` back to `true`
  on every spec instance (silently re-enabling Gitea on v6). Fixed + regression
  test (`TestRenderValues_GiteaDisabledByDefault`).

**Compatibility check — SOPS removal + SealedSecrets manifests dir (INVESTIGATED
2026-07-03, no blocker).** v6 deletes `kms.sops` and the operator writes
SealedSecrets into `env/manifests/namespaces/apl-secrets/` (+ `apl-users/`,
`env/manifests/global/`; defaults `GITOPS_{NS,GLOBAL}_MANIFESTS_RELATIVE_PATH =
env/manifests/{namespaces,global}`) in the values repo.

- **(a) SOPS reliance — CLEAN.** The landing zone never sets a `kms:` block
  (grep: no `kms:` in `apl-values/`); its own secrets are OpenBao+ESO. The
  `apl_sops_secrets_placeholder` TF resource is already removed (v6 made the
  operator's envFrom `optional: true`). Remaining `sops` hits are inert: the
  `import` tooling reads a *foreign* 5.x cluster's SOPS values (legitimate), and
  `secrets.md`'s "without a KMS" line is about OpenBao's static seal key, not
  SOPS. One stale reference — `AplCoreChain()` in `tools/internal/terraform/
  untrack.go` still listed the removed placeholder — was **harmless** (`stateRm`
  skips addresses not in state) and has been dropped.
- **(b) manifests-dir collision — NONE.** The operator writes a **top-level
  `env/` tree** (`otomi.git.path` is schema-forbidden, so it operates on a fixed
  location and pushes `env/...`), disjoint from the landing zone's
  `apl-values/<env>/manifest/` — different top-level dir *and* singular-vs-plural.
  The instance repo tracks no top-level `env/` or values-repo `manifests/` path.
  The operator's `gitops-global`/`gitops-<ns>` ArgoCD Apps (project `default`,
  syncing `env/manifests/*`) don't overlap the landing zone's `platform-bootstrap`
  App (project `platform-support`, syncing `apl-values/<env>/manifest`). The push
  is `git add -A` + `checkout -B` + `push` (no `--force`) with pull/conflict retry
  on a full clone, so it *adds* `env/` and never prunes `apl-values/`.

**Residual lab-confirm (below):** on a live v6 bootstrap, verify the operator's
`env/` tree + its `gitops-*` Applications actually materialize and go Healthy
alongside `platform-bootstrap` (the two Argo trees coexisting is the one thing not
provable statically). The landing zone uses OpenBao+ESO and does not adopt
SealedSecrets; apl-core's own platform secrets ride its `env/manifests/apl-secrets`
path, unsealed by the (enabled) sealed-secrets controller.

## Lab-validation checklist (must pass before promoting past lab)

These could not be verified statically against an RC; confirm on a live v6 lab:

- [ ] apl-core's ESO controller SA name is exactly `external-secrets` in the
      `external-secrets` namespace (`kubectl -n external-secrets get sa`). If
      apl-core suffixes the fullname, update `ci_openbao_configure.go` +
      both ClusterSecretStores to match.
- [ ] ESO mints the `serviceAccountRef` token (the chart ClusterRole carries
      `serviceaccounts/token` create — confirmed in source) and the OpenBao
      `eso`/`eso-pusher` roles authenticate.
- [ ] The ESO webhook pod label still matches the ingress NetworkPolicy
      selector (`app.kubernetes.io/name: external-secrets-webhook`).
- [ ] The wave -10 ESO NetworkPolicies + generated-secrets in the
      apl-core-created `external-secrets` namespace only cause transient Argo
      retries on first boot (the namespace is created by apl-core's helmfile,
      not pre-created at -20).
- [ ] `sealed-secrets.enabled` stays true (it gates apl-core's ESO install).
- [ ] **SealedSecrets manifests-dir coexistence** (static analysis found no
      collision — see the investigated watch-out above): confirm on the live
      cluster that the operator's top-level `env/` tree + its `gitops-global` /
      `gitops-<ns>` Applications materialize and go Healthy alongside
      `platform-bootstrap`, and that apl-core's own platform SealedSecrets under
      `env/manifests/namespaces/apl-secrets` unseal (sealed-secrets controller up).
- [ ] Gitops converges against the external `otomi.git` repo with gitea off and
      git-server default-on.
- [ ] Loki gateway comes up (promtail removed / Loki chart restructured) — the
      nginx-resolver render still works.
- [x] **Loki admin password.** RESOLVED by the e2e (2026-07-03): omitting
      `adminPassword` fails the helm-install schema (`required`), so it is supplied
      again from a TF `random_password` (see "What we implemented" #6). Remaining
      live check: grafana's loki datasource authenticates against the gateway with
      the rendered password.
- [ ] Gateway-API CRDs are provisioned (ingress-nginx removed).
- [ ] CoreDNS `*.internal` rewrite: inventory sidecar-less/init consumers and
      narrow to an explicit host allowlist (details under
      [GA follow-ups](#ga-follow-ups-v600-source-review-2026-07-03) — keycloak
      MUST stay for the oauth2-proxy init container; the "harbor.internal remains"
      grep was a false positive).
- [ ] PSS `restricted` vs sidecar injection unchanged (ambient stays off).

## Migration axes: template vs downstream instances

**Template (this repo).** The code changes above. Land them, validate in a lab
instance, hold past-lab promotion until the lab-validation checklist passes.

**Downstream instances** (generated repos + their live clusters):

1. `copier update` each instance repo to pull the template changes.
2. Bump the sha-pinned e2e `TF_IMAGE` if the llz contract changed (see the
   `e2e TF_IMAGE coupling` note).
3. Per-cluster **in-place upgrade**, not fresh install: bump `apl_chart_version`
   in the cluster's `cluster-bootstrap/<env>.tfvars` → `terraform apply` →
   `helm_release.apl` upgrades apl-operator → its helmfile re-converges the
   platform. Watch apl-operator logs for the helmfile pipeline.
4. Promote lab → staging → primary → secondary, one production region at a time
   (per the [apl-core-migration-runbook](../apl-core-migration-runbook.md)).

**Upgrade-specific watch-outs** (beyond the lab checklist):

- On upgrade, removing the self-managed ESO means the old `llz-external-secrets`
  controller + namespace are pruned by Argo/Terraform. Confirm no ExternalSecret
  is briefly orphaned during the cutover (apl-core's ESO must be Ready first).
- Removing the `apl_sops_secrets_placeholder` resource makes `terraform apply`
  delete the orphaned `apl-sops-secrets` Secret — safe on 6.x (envFrom optional).
- Gitea disablement on an existing cluster: apl-core should reap the gitea
  release; confirm gitops re-targets the external repo without a flap.
