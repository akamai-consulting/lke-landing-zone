---
name: netpol-change
description: Writing or changing a NetworkPolicy, or debugging a hang that might be one. Use when adding a policy to a chart or platform-apl component, when a pod is Running-and-healthy but something cannot reach it, when converge stalls on an APIService, or when mesh-egress-guard / monitoring-label-guard trips. Cilium on LKE-E has traps a correct-looking policy walks straight into.
---

# Changing a NetworkPolicy

**A NetworkPolicy that matches nothing looks exactly like a healthy cluster.**
Every scar below presented as `1/1 Running` with healthy endpoints and a hang
somewhere unrelated. The scars section of
[`docs/lessons-learned.md`](../../../docs/lessons-learned.md) is canonical — read
it before writing the policy, not after the hang.

## Before you write it

Answer these four. Three of the four have burned a full e2e cycle.

### 1. Which port does Cilium see?

**Cilium evaluates policy post-DNAT.** Egress to the kube-apiserver must allow
`6443` alongside `443`, or every API call is silently dropped. The rule
generalises: **match the target port, not the service port** — `443 → 6443`,
`80 → 8080`.

### 2. Does the selector match a pod that exists *today*?

Upstream relabels are the most common way a policy goes to zero matches, and
nothing reports it:

- argo-events v1.9+ carries `controller=*-controller`, not
  `app.kubernetes.io/component=*` → JetStream loops on "Waiting for routing".
- apl-core 5.0.0 Gateway pods are labeled
  `gateway.networking.k8s.io/gateway-name=<name>`, not legacy `app=gateway-<name>`
  → intra-cluster HTTPS hangs while the external LB still works.

Check the live labels rather than the labels the chart used to emit.

### 3. Is the peer an aggregated APIService?

**A core-webhook allow does not cover an aggregated one.** The Linode DNS-01
webhook apl-core deploys is an aggregated APIService labeled
`app=cert-manager-webhook-linode` serving `:443` — not covered by the core
cert-manager webhook allow, so the apiserver's discovery probe is dropped, the
APIService stays `Available=False` with `FailedDiscoveryCheck`, and `llz ci
converge` polls forever. The pod is `1/1 Running` with healthy endpoints
throughout.

> **General rule: any aggregated-APIService webhook behind a default-deny needs
> its own `:443` ingress allow keyed on *its* pod label.**

### 4. Will it be enforced before the pods it protects start?

Helm-templated policies land in the same Argo wave as their pods, and cilium-agent
BPF programming is async — so a workload with a short retry-then-fatal loop
crashloops before the policy enforces. **Annotate NetworkPolicies with
`sync-wave: "-10"`.**

## The default-deny namespaces have known holes

Only some namespaces carry a default-deny, and the ones that do have each needed a
specific allow added after an outage:

- **ESO's default-deny has no matching allow-ingress**, so the validating webhook
  times out on every `ClusterSecretStore`/`ExternalSecret` mutation.
- **`harbor-default-deny` blocks the CNPG operator** — no `cnpg-system` allow, so
  the operator cannot poll Postgres status, the Harbor DB replica never starts, and
  convergence stalls.
- **An Istio sidecar needs istiod egress** (`tcp/15012` to `istio-system/app=istiod`)
  in a default-deny namespace. Without it the iptables redirect installs, Envoy
  aborts with no cert, and **all** pod egress dies — including to the apiserver.
  It surfaces as cryptic "connection refused" crashloops; the fix is two lines of
  NetworkPolicy.

## The mesh is a second, invisible policy layer

apl-core runs platform namespaces (harbor) under Istio **STRICT** mTLS. A pod
**outside** that mesh cannot reach a Service inside it — dropped at the sidecar,
not by NetworkPolicy, so no NetworkPolicy edit will ever fix it.

`mesh-egress-guard` flags any policy egress into a STRICT-mesh namespace from a
different namespace, which is exactly the mistake it was built from. It **depends
on `render-charts`** — first-party policies are only visible once rendered — and
hard-fails on a missing rendered tree rather than passing green over a corpus it
never saw.

Note the asymmetry: Harbor's *PeerAuthentication* ships PERMISSIVE by design at
[ADR 0010](../../../docs/adr/0010-in-cluster-mtls.md) step 3. Read the live mode
rather than assuming.

## Where policies live

| Tree | Path |
|---|---|
| Cluster foundation chart | [`kubernetes-charts/llz-cluster-foundation/templates/network-policies.yaml`](../../../kubernetes-charts/llz-cluster-foundation/templates/network-policies.yaml) |
| Cert automation chart | [`kubernetes-charts/llz-cert-automation/templates/network-policies.yaml`](../../../kubernetes-charts/llz-cert-automation/templates/network-policies.yaml) |
| Per-component overlays | `platform-apl/components/*/network-policies.yaml` |

`platform-apl/` is **not** covered by `k8s-lint`, `k8s-validate` or the kind
dry-run — all three read the rendered chart output only. `dropped-apiversions`
is the gate that walks it, which is how an ExternalSecret shipped on an
apiVersion apl-core had stopped serving.

## Verifying

```bash
make render-charts
make mesh-egress-guard
make k8s-lint
```

Static checks cannot tell a policy that matches nothing from one that matches
correctly. If the change is load-bearing for reachability, it needs a live gate —
see the `gate` skill, archetype **unverified delivery**: assert at the consumer,
on traffic the producer really sent. `llz ci net-probe` classifies each dial as
refused / timeout / dns, and those three point at different subsystems — keep the
distinction in whatever you build on top of it.
