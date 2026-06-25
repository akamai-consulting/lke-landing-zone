# llz-openbao-platform

> **Cutover status: live (consumed via OCI Argo Application).** The cluster's
> `platform-openbao` Application now sources this chart from GHCR
> (`apl-values/components/openbao/openbao.yaml`); the old
> in-repo chart machinery was removed and the `OPENBAO_CHART` Makefile targets +
> per-env `replacements:` were repointed/cleaned. HA-Raft boots fresh on the
> recreated cluster. `releaseName: platform-openbao` is preserved — the
> StatefulSet/Service/cert SANs/raft `retry_join` FQDNs and the Application's
> `ignoreDifferences` all derive from it.

Opinionated "OpenBao on Kubernetes, done right" wrapper. It wraps the upstream
[OpenBao Helm chart](https://openbao.github.io/openbao-helm) (HA integrated-Raft,
3 replicas) as a subchart dependency and layers on the cluster integration the
bare chart leaves to you:

- **cert-manager serving TLS** — a `Certificate` whose SANs cover every per-pod
  raft FQDN, issued by a *stable* bootstrap CA so the serving CA never rotates
  mid-bootstrap and raft TLS forms on first boot.
- **NetworkPolicies** — default-deny plus an explicit allow-list of client
  namespaces on `:8200`, the intra-raft `:8200/:8201` mesh, DNS, the K8s API
  (with the LKE-Enterprise `443→6443` DNAT quirk), and audit egress to Loki.
- **Prometheus ServiceMonitor** — scrapes `/v1/sys/metrics` over HTTPS.
- **Promtail audit-shipper** — a sidecar config that tails the file audit device
  and ships to the in-cluster Loki gateway.
- **AppRole-rotation CronWorkflow** — an Argo CronWorkflow (+ SA/RBAC/ESO) that
  rotates the AppRole `secret_id` used by CI and ESO on a quarterly schedule.

## Why this chart exists

OpenBao's own chart gets you a StatefulSet and Services, but every team then
re-discovers the same scars to make it production-safe on a locked-down cluster.
This chart captures them as **defaults**:

- **Raft join ordering** — `retry_join` blocks for all peers, with the per-pod
  FQDNs present as cert SANs so TLS forms before bootstrap.
- **Pod Security Standards** — the `restricted:v1.31` securityContexts the
  StatefulSet needs to schedule at all.
- **Liveness during bootstrap** — `sealedcode=204&uninitcode=204` so a freshly
  deployed (sealed) OpenBao isn't SIGKILLed before `bao operator init` runs.
- **LKE-Enterprise NetworkPolicy** — `6443` allowed alongside `443` because the
  `kubernetes` Service DNATs `443→6443` and Cilium enforces on the post-DNAT port.
- **Audit HCL** — explicit `type = "file"` (OpenBao 2.5.0's parser won't infer
  it) and `mode = "0640"` so the Promtail sidecar can read the audit log.

## Decoupling (what's a value vs. a default)

Linode + apl-core assumptions stay as
**defaults**; only org/cluster identity is variabilized. The newly-decoupled
knobs live under `platform` and `openbaoPromtail`:

| Key | Default | Notes |
|---|---|---|
| `platform.releaseName` | `platform-openbao` | **Load-bearing.** StatefulSet/Service identity; cert SANs and raft FQDNs assume it. |
| `platform.internalServiceName` | `platform-openbao-internal` | **Load-bearing.** Headless Service raft peers resolve through. |
| `platform.tls.secretName` | `openbao-tls` | **Load-bearing.** Mounted at `/openbao/tls`; watched by `openbao-cert-watcher`. |
| `platform.tls.issuerRef.name` | `openbao-ca` | cert-manager issuer (stable self-signed bootstrap CA). |
| `platform.tls.issuerRef.kind` | `ClusterIssuer` | |
| `platform.tls.duration` / `renewBefore` | `8760h` / `720h` | |
| `platform.networkPolicy.enabled` | `true` | |
| `platform.networkPolicy.allowedClientNamespaces` | `[llz-external-secrets, llz-cert-automation, llz-observability]` | Namespaces allowed to reach `:8200`. |
| `platform.networkPolicy.observabilityNamespace` | `observability` | Audit egress target on `:80`. |
| `platform.serviceMonitor.enabled` | `true` | Decoupled from the old `Release.Name == "platform-prom"` magic gate. |
| `platform.serviceMonitor.releaseLabel` | `platform-prom` | The `release:` label the Prometheus Operator selects on. |
| `openbao.server.ha.replicas` | `3` | Raft replica count (passed through to the subchart). |
| `openbaoPromtail.lokiPushUrl` | `http://loki-gateway.observability.svc.cluster.local/loki/api/v1/push` | |
| `openbaoPromtail.region` / `cluster` | `primary` / `platform-openbao` | Audit log labels. |

> **Changing `openbao.server.ha.replicas` is not a single-value edit.** The
> `retry_join` blocks in `openbao.server.ha.raft.config` and the cert SANs in
> `templates/openbao-tls-cert.yaml` enumerate `platform-openbao-0..2` explicitly.
> A non-3 replica count means editing the retry_join list too (the cert SANs
> auto-range off the replica count).

## Install

```sh
helm dependency build kubernetes-charts/llz-openbao-platform
helm install platform-openbao oci://ghcr.io/akamai-consulting/charts/llz-openbao-platform \
  --version 0.1.0 \
  -n openbao --create-namespace \
  --set approleWorkflow.approleRotationEnabled=true
```

> Use release name `platform-openbao` (or set `platform.releaseName` to match your
> chosen release name) so the StatefulSet identity, cert SANs, and raft
> retry_join all line up.

In this repo it is consumed by an Argo CD Application
(`apl-values/components/openbao/`) referencing the
published OCI chart. OpenBao manages stateful PKI, so that Application keeps
`prune: false`, `selfHeal: true`, and the `ignoreDifferences` for the
StatefulSet `volumeClaimTemplates` + the ESO `deletionPolicy` defaulting.

## Bootstrap

After first sync the pods come up **sealed**. Bootstrap is automated — run
`.github/workflows/bootstrap-openbao.yml` for each region (init, unseal, Raft
join, KV v2, AppRole, Kubernetes auth, policies, audit, secret seeding).
