# llz-cluster-foundation

The secure-by-default baseline every namespace on a Linode LKE-E + apl-core
cluster needs. apl-core (Akamai App Platform) gives you Istio, Argo CD,
cert-manager, Harbor, and Keycloak — but it ships a default-permit posture, no
per-app NetworkPolicies, and several gaps that only surface as CrashLoopBackOff,
`ClusterIsNotReady`, or NXDOMAIN on a fresh bootstrap. This chart closes those
gaps as **correct-by-default**.

## What it deploys

1. **Namespaces apl-core does NOT manage** — `llz-openbao`, `grafana`,
   `monitoring`, `llz-observability`, `llz-argo-workflows`, `llz-argo-events`.
   (`external-secrets` and `linode-volume-labeler` are NOT created here: ESO
   ships with apl-core 6.x, and the volume labeler was retired into the
   `llz reconcile` volume-labels lane — re-declaring it pinned platform-bootstrap
   OutOfSync.) Each with restricted Pod Security Standards where
   appropriate; a few are pre-created purely to win a cold-bootstrap race against
   apl-core's later create-or-replace (server-side-apply merges cleanly). Sync
   wave `-20` so they exist before anything targets them.

2. **Default-deny NetworkPolicies + minimum-viable allows** for the
   apl-core-managed namespaces apl-core ships no NPs into (`cert-manager`,
   `harbor`, `istio-system`, `observability`). This is where the operational
   scars become defaults — see below. Sync wave `-10`.

3. **CoreDNS `*.internal` rewrite** (`coredns-custom` ConfigMap, wave `-10`) to
   the platform Istio gateway Service, so non-meshed pods can resolve apl-core's
   ServiceEntry hostnames (`keycloak.internal`, `harbor.internal`, …) that
   otherwise return NXDOMAIN.

4. **One-shot CoreDNS rollout-restart** (`coredns-restart-on-custom-cm` Job, wave
   `-9` + `PostSync` hook) — CoreDNS's `reload` plugin watches the Corefile mtime
   but not the import-dir, so the include file is silently ignored until a single
   restart after it first lands.

5. **Default StorageClass demotion** (`sc-default-patcher` Job, `PostSync` hook)
   — demotes LKE's `linode-block-storage-retain` so the encrypted/tagged
   `block-storage-retain` becomes the cluster default. Re-runs on
   every sync (incl. selfHeal), so it self-recovers if Flux reverts the demotion.

## The NetworkPolicy scars (now defaults)

Every allow rule here exists because a default-deny without it broke something:

- **LKE-E apiserver dual-port 443 + 6443.** On LKE-E the apiserver is DNAT'd;
  Cilium sees `:6443` post-rewrite. Both ports, no `to:` selector — the managed
  control plane has no in-cluster `kube-apiserver` pod, so a podSelector would
  match nothing and silently block all apiserver egress.
- **istiod xDS egress (15010/15012/15014).** Sidecar-injected pods must reach
  istiod or Envoy's iptables redirect kills *all* egress (including apiserver)
  and the workload never reaches Ready.
- **CNPG operator + Prometheus allow in `harbor`.** Without the cross-namespace
  `:8000`/`:9187` allows, the CloudNativePG operator's health poll fails,
  `harbor-otomi-db` stays `ClusterIsNotReady`, and convergence stalls.
- **Gateway-API pod selector.** Gateway pods are labeled
  `gateway.networking.k8s.io/gateway-name: platform`, not the legacy
  `app: gateway-<name>` — selecting the wrong label matches nothing.

## Install

```sh
helm install llz-cluster-foundation oci://ghcr.io/akamai-consulting/charts/llz-cluster-foundation \
  --version 0.1.13
```

In this repo it is consumed by an Argo CD Application at an early sync wave
(foundation must land before workloads).

## Values

| Key | Default | Description |
|---|---|---|
| `nameOverride` / `fullnameOverride` | `""` | Affect chart-level labels only; managed resource names are literal contracts and never prefixed. |
| `image.repository` | `alpine/k8s` | Image for the patcher Jobs (needs `kubectl` + shell). |
| `image.tag` | `1.32.4` | Image tag. |
| `image.pullPolicy` | `IfNotPresent` | |
| `syncWaves.namespaces` | `"-20"` | Namespaces land before anything targeting them. |
| `syncWaves.networkPolicies` | `"-10"` | NPs before workload pods. |
| `syncWaves.corednsCustom` | `"-10"` | CoreDNS ConfigMap before pods that wait on `*.internal`. |
| `syncWaves.corednsRestart` | `"-9"` | CoreDNS restart Job + its RBAC/NP. |
| `podSecurityStandardsVersion` | `"v1.33"` | PSS version pin (match the cluster K8s minor). |
| `namespaces` | see values.yaml | List of `{name, restricted, labels}` for namespaces apl-core doesn't manage. |
| `networkPolicies.enabled` | `true` | Master toggle for the foundation NP set. |
| `networkPolicies.apiserverPorts` | `[443, 6443]` | LKE-E apiserver egress port pair. |
| `networkPolicies.dnsPorts` | `[53/UDP, 53/TCP]` | DNS egress. |
| `networkPolicies.istiod` | `istio-system` / `app: istiod` / `[15010,15012,15014]` | istiod selector + xDS egress ports. |
| `networkPolicies.gateway` | `gateway-name: platform`, ingress `[80,443,15021]` | Platform Istio gateway selector + ingress ports. |
| `networkPolicies.istiodIngressPorts` | `[15010,15012,15014,15017]` | istiod ingress ports. |
| `networkPolicies.cnpg` | see values.yaml | CNPG instance selector + operator/metrics allows in `harbor`. |
| `networkPolicies.grafanaNamespace` | `grafana` | Allowed ingress source into `llz-observability`. |
| `networkPolicies.harborMetrics` | see values.yaml | Allows the `monitoring` namespace to scrape Harbor metrics. |
| `networkPolicies.otelMetrics` | see values.yaml | Allows the `monitoring` namespace to scrape the OTel Collector. |
| `coreDNS.custom.enabled` | `true` | Toggle the `coredns-custom` ConfigMap. |
| `coreDNS.custom.matchSuffix` | `internal` | Domain suffix the rewrite matches. |
| `coreDNS.custom.gatewayTarget` | `platform-istio.istio-system.svc.cluster.local.` | Rewrite target (platform Istio gateway Service FQDN). |
| `coreDNS.restart.enabled` | `true` | Toggle the one-shot restart Job. |
| `coreDNS.restart.targetDeployment` | `workload-coredns` | CoreDNS Deployment to restart. |
| `coreDNS.restart.rolloutStatusTimeout` | `180s` | Rollout-status wait. |
| `coreDNS.restart.availableTimeout` | `60s` | Available-condition wait. |
| `storageClassPatcher.enabled` | `true` | Toggle the default-SC demotion Job. |
| `storageClassPatcher.desiredDefault` | `block-storage-retain` | Class promoted to default (TF-rendered). |
| `storageClassPatcher.demote` | `linode-block-storage-retain` | LKE default to demote. |
| `*.resources` | see values.yaml | Container resources for the Jobs. |
