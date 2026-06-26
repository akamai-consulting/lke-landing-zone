# apl-core migration runbook

This runbook covers the operational procedure for cutting a cluster over to
**Akamai App Platform (apl-core)**. It assumes you have already read the
architecture rationale in the [adopter guide](adopter-guide.md) and have access
to all the credentials listed in the [bootstrap-openbao runbook](runbooks/bootstrap-openbao.md)
and the [Linode account request checklist](infosec/linode-account-request-checklist.md).

The cutover happens **per cluster**, not all at once. The promotion path is
**lab → staging → primary → secondary** (rename to match your own `<env>` set).

## Phase 0 — Prerequisites (one-time)

- [ ] DNS zone for the per-env hostnames already exists in Linode DNS
      (`<cluster_domain>` from the per-env tfvars). Apl-core ExternalDNS
      creates A records under that zone; the zone itself must pre-exist.
- [ ] `LINODE_DNS_TOKEN` GitHub secret seeded — apl-core's ExternalDNS
      and cert-manager DNS-01 solver both use it. Without it ACME
      challenges fail and TLS never issues.
- [ ] **DNS-01 webhook — no action needed (apl-core owns it).** apl-core
      deploys `cert-manager-webhook-linode` as part of its cert-manager
      integration, registering the `acme.slicen.me` API group (the slicen chart
      default) and holding the Linode token from `LINODE_DNS_TOKEN` above. The
      landing zone no longer ships its own webhook Application; the
      `llz-letsencrypt-{production,staging}` ClusterIssuers
      ([`apl-values/_shared/manifest/dns/letsencrypt-clusterissuer.yaml`](../instance-template/apl-values/_shared/manifest/dns/letsencrypt-clusterissuer.yaml))
      target that group via `groupName: acme.slicen.me` + `solverName: linode`.
      Just confirm apl-core's `cert-manager-webhook-linode` pod reaches Ready
      (its APIService `v1alpha1.acme.slicen.me` shows `Available=True`) — if it
      doesn't, every Let's Encrypt Certificate sits in Pending and no Istio
      Gateway gets TLS.
- [ ] **Verify the apl chart version** — run
      `helm repo add apl https://linode.github.io/apl-core && helm repo update && helm search repo apl/apl --versions | head`
      and update `apl_chart_version` in each
      `instance-template/terraform-iac-bootstrap/cluster-bootstrap/<env>.tfvars` to match.
      The current pin (`5.0.0`) is provisional.
- [ ] **Mirror `apl-values/` to an HTTPS-reachable Git host** (the placeholder
      is `https://github.com/<org>/apl-values.git`). apl-core's values
      schema enforces `^https?://.+` on the values-repo URL; SSH is not
      supported. A `github.com` that requires per-cluster node-IP allowlisting
      cannot satisfy LKE-E, so the values tree cannot live there for apl-core
      to read.
      Recommended: a github.com private repo synced from your primary Git host
      via a CI job, or an internal HTTPS mirror.
- [ ] Update `apl_values_repo_url` + `apl_values_repo_username` in every
      `instance-template/terraform-iac-bootstrap/cluster-bootstrap/<env>.tfvars` if the
      placeholder values don't match your environment.
- [ ] Note that OpenBao's KV-v2 mount, Kubernetes auth, and policies are
      configured by `llz ci bao-configure` (run from `bootstrap-openbao.yml`
      after the cluster is up), not by a Terraform root and not by
      `cluster-bootstrap/`.

## Phase 1 — Lab cutover (the rehearsal)

The lab cluster is disposable. Use it to shake out anything that's still
hand-rolled or that doesn't fit apl-core's defaults before touching staging.

1. **Bump node count** in the cluster tfvars for your lab `<env>`
   (`instance-template/terraform-iac-bootstrap/cluster/<env>.tfvars`) to 3 — apl-core
   minimum is 3 × 8GB/4vCPU.

2. **Apply cluster + object-storage Terraform**:
   ```bash
   cd instance-template/terraform-iac-bootstrap/cluster
   terraform apply -var-file=<env>.tfvars
   cd ../object-storage
   terraform apply -var-file=<env>.tfvars
   ```
   Copy `LOKI_S3_ACCESS_KEY` / `LOKI_S3_SECRET_KEY` outputs into the
   `infra-<env>` GitHub environment secrets.

3. **Apply cluster-bootstrap Terraform** — this is the apl-core install:
   ```bash
   cd ../cluster-bootstrap
   terraform apply -var-file=<env>.tfvars
   ```
   The `helm_release.apl` resource installs apl-core. Apply blocks until
   the `apl-operator` deployment is Ready (timeout 900s).

4. **Watch the helmfile pipeline** in apl-operator logs:
   ```bash
   kubectl logs -n apl-operator -l app.kubernetes.io/name=apl-operator -f
   ```
   Expect 10-15 minutes of helmfile activity. apl-core installs ~40 components
   in dependency order: namespaces → Kyverno → Sealed Secrets → ESO → cert-manager
   → CNPG → kube-prometheus-stack/Grafana/Loki → Istio → Keycloak/Gitea/Harbor/Argo CD
   → apl-api/apl-console (apl-core's bundled Tekton chart is disabled in the
   per-env values.yaml — cert-automation runs on Argo Workflows + Events).

5. **Get the Console URL and admin credentials**:
   ```bash
   kubectl get configmap welcome -n apl-operator -o jsonpath='{.data.consoleUrl}'
   kubectl get secret platform-admin-credentials -n keycloak \
     -o jsonpath='{.data.username}' | base64 -d ; echo
   kubectl get secret platform-admin-credentials -n keycloak \
     -o jsonpath='{.data.password}' | base64 -d ; echo
   ```

6. **Verify the `manifest/` tree reconciles.** Apl-core's in-cluster
   Argo CD picks up the values repo (configured via `otomi.git.path: apl-values/<env>`)
   and applies the manifests recursively. The lab cluster will sync from the
   shared manifest base (via the per-env `manifest/kustomization.yaml`)
   plus the per-env `manifest/ingress/` overlay.
   ```bash
   kubectl -n argocd get applications
   ```
   Expect to see the AppProjects (e.g. `<project>-support`) plus Applications
   for OpenBao, cert-manager-webhook-linode, cert-manager extras, the
   internal-CIDR firewall controller (suspended), and any others you ship.

   > **The first 10-15 minutes will be noisy.** Several Applications ship
   > resources whose CRDs are installed by apl-core itself
   > (ServiceMonitor + PrometheusRule via kube-prometheus-stack;
   > Gateway, VirtualService, Certificate via Istio + cert-manager;
   > ExternalSecret + ClusterSecretStore via ESO; Workflow, WorkflowTemplate,
   > EventBus, EventSource, Sensor via the Argo Workflows + Argo Events
   > Applications shipped from this template). Until apl-core's helmfile
   > finishes those installs, Argo CD reports `no matches for kind …` for
   > each affected Application. This is **expected and self-healing** —
   > Argo CD retries with exponential backoff. Wait for the apl-operator
   > log line `helmfile-91.artifacts.yaml.gotmpl: completed` (the last
   > helmfile stage) before treating any Application Degradation as real.

7. **Bootstrap OpenBao**:
   ```bash
   gh workflow run bootstrap-openbao.yml -f environment=<env> -f mode=init
   ```
   Copy the static seal key (`OPENBAO_SEAL_KEY`) + recovery keys 4-5 + root token to offline storage. See
   [docs/runbooks/bootstrap-openbao.md](runbooks/bootstrap-openbao.md).

8. **Verify ESO ClusterSecretStore + downstream ExternalSecrets** are syncing:
   ```bash
   kubectl -n external-secrets get clustersecretstore openbao
   kubectl get externalsecret -A
   ```

9. **Verify Istio Gateway routes**:
   ```bash
   kubectl -n observability get gateway,virtualservice
   kubectl -n harbor get gateway,virtualservice
   curl -kI https://otel.<env>.<cluster_domain>/v1/metrics    # expect 401 (no auth) or 200 (with token)
   curl -kI https://harbor.<env>.<cluster_domain>/v2/         # expect 401
   ```

10. **Trigger a cert-automation rehearsal** (if your product ships a
    certificate-rebuild Workflow):
    ```bash
    # Force a Secret resourceVersion bump
    kubectl -n cert-manager annotate secret <tls-secret> force-rebuild=$(date +%s) --overwrite
    # The EventSource watch should fire ~immediately; a rebuild Workflow
    # lands in cert-automation within seconds.
    kubectl -n cert-automation get workflow
    ```

If anything in steps 6-10 is unhealthy, iterate on `apl-values/<env>/` and
re-apply. The Argo CD reconciler does the rest.

## Phase 2 — Staging

Repeat all of Phase 1 against the staging cluster using `staging.tfvars`.
Sign off the staging runbook before moving to production.

## Phase 3 — Production cutover

If your topology tolerates a single region being down (e.g. a dual-region
setup that absorbs traffic from the other side), take advantage of this to cut
over one production region at a time.

1. **Primary**: repeat Phase 1 steps 2-10 with `primary.tfvars`.
2. **Secondary**: repeat with `secondary.tfvars`. If a region carries an
   additional Istio Gateway for OpenBao, it lives under that env's
   `manifest/ingress/openbao-gateway.yaml`.
3. **Update the secondary OpenBao address placeholder** in the primary env's
   `values.yaml` with the secondary OpenBao Gateway hostname, then re-sync.
4. **Run cross-region consistency check** — read a known key from each region and
   compare (a divergence means a dual-write didn't land):
   ```bash
   diff <(llz openbao get active   secret/<project>/keys <app_secret>) \
        <(llz openbao get standby secret/<project>/keys <app_secret>) && echo "in sync"
   ```

## Phase 4 — Retire dead code

Once all clusters are green for at least one full cycle (cert renewal,
cert-automation Workflow firing), delete
any legacy paths left over from a pre-apl-core deployment.

## Rollback

If apl-core itself is the problem (not your manifests), the rollback path is
**recreate the LKE-E cluster** — apl-core has no clean uninstall path because
it manages 40+ namespaces with finalizers. Restore by:

1. `terraform destroy` in `cluster-bootstrap/` to remove the values-repo Secret.
2. `terraform destroy` in `cluster/` to remove the LKE-E cluster.
3. Roll the working tree back to before the migration change.
4. Re-apply `cluster/` then `cluster-bootstrap/` to rebuild the bootstrap
   Argo CD install.

Production cutover gate: **lab + staging green for at least one full chaos
cycle** (delete a node, kill the OpenBao leader, force a cert-rebuild
Workflow) before touching primary.

## Carried-forward known issues

| Issue | Status |
|---|---|
| cert-manager-webhook-linode chart URL placeholder | Resolve before lab apply succeeds with Let's Encrypt prod issuer |
| Secondary OpenBao address | Updated after Phase 3 step 2 |
| mTLS CA in K8s Secret rather than OpenBao PKI engine | Per your org's policy — PKI backlog |
| OTLP log pipeline disabled | Tracked |
| apl-core apiserver NetworkPolicies vs LKE-E post-DNAT port 6443 | Audit during Phase 1; patch via `manifest/` if needed |
| PSS `restricted` vs Istio sidecar injection | Verify during Phase 1; switch to `baseline` or selective injection if it breaks |
| Keycloak federation deferred | Non-blocking |
