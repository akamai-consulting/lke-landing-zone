# Design: apl-core 5.x → 6.x migration

**Status:** In progress — code landed on branch `feat/apl-core-v6-migration`;
**v6 is pre-release** (latest tag `v6.0.0-rc.12`), so everything here is gated on
lab validation before any non-lab promotion.
**Relates to:** [apl-core-migration-runbook.md](../apl-core-migration-runbook.md),
[../secrets.md](../secrets.md), [linode-credential-rotator.md](linode-credential-rotator.md),
`instance-template/apl-values/`, `instance-template/terraform-iac-bootstrap/cluster-bootstrap/`,
`tools/cmd/llz/ci_openbao_configure.go`.

## Why

The landing zone pins the apl-core Helm chart at `5.0.0` (provisional). apl-core
6.x reworks several platform components the landing zone integrates with. This
design captures what changed, what we did to the **template**, and how to
**upgrade downstream instances** (generated repos + their live clusters).

> **GA status.** As of this writing there is no stable `6.0.0` — only release
> candidates (`v6.0.0-rc.12`, the pin we use). The latest *stable* line is
> `5.1.x`. Treat the v6 work as lab-only until 6.0.0 GAs, then re-pin.

## What changed in v6 (evidence from the `v5.0.0…v6.0.0-rc.12` source diff)

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

1. **Pin bump** → `6.0.0-rc.12` (tfvars example, spec env example, adopter guide).
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
6. **Loki admin password → apl-core-managed.** v6 made `apps.loki.adminPassword`
   an x-secret with a generator (`x-secret: '{{ randAlphaNum 20 }}'`) and sources
   the loki reverse-proxy auth Secret from apl-core's `core-secrets-store`. So we
   stopped supplying it: removed the `loki_admin_password` TF variable + templatefile
   input, the `LOKI_ADMIN_PASSWORD` GitHub env secret, the `ensure-env-secret`
   workflow step, the destroy-path TF_VAR wiring, the token/state inventories
   (`tokens.go`, `state.go`), and the related docs. This resolves the former
   "Known limitation — Loki admin password" in [secrets.md](../secrets.md). (The
   ESO push-to-OpenBao pattern used for grafana/otel does NOT fit here — apl-core
   owns the loki auth Secret, and the value is needed at render time, which a
   runtime-generated ESO secret can't provide.)

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
- [ ] Gitops converges against the external `otomi.git` repo with gitea off and
      git-server default-on.
- [ ] Loki gateway comes up (promtail removed / Loki chart restructured) — the
      nginx-resolver render still works.
- [ ] **Loki admin password x-secret persists.** With `adminPassword` omitted,
      apl-core must auto-generate it AND persist it stably (via its secret backend
      / core-secrets-store) so it does not churn on each reconcile. Confirm the
      loki reverse-proxy ExternalSecret resolves and grafana's loki datasource
      authenticates. If apl-core's x-secret persistence is unreliable in this
      BYO-git setup, the fallback is to supply `adminPassword` explicitly again
      (revert commit) — there is no clean ESO-generated middle ground for a
      render-time values field.
- [ ] Gateway-API CRDs are provisioned (ingress-nginx removed).
- [ ] CoreDNS `*.internal` resolution still needed (5.1.0 ServiceEntry fix may
      reduce the rewrite workaround).
- [ ] PSS `restricted` vs sidecar injection unchanged (ambient stays off).

## Migration axes: template vs downstream instances

**Template (this repo).** The code changes above. Land them, validate in a lab
instance, hold past-lab promotion until GA.

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
