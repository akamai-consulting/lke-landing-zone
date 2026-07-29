# ADR 0010 — Mutual TLS for in-cluster communication

Status: **Proposed** — implemented but NOT validated on a live cluster.
Date: 2026-07-28
Supersedes the "in-cluster posture" rationale in `tools/internal/openbao/openbao.go`
(`HTTPClientInsecure`) and the `OPENBAO_SKIP_VERIFY` comments across the workload manifests.

## Context

An audit of in-cluster traffic found that **nothing in the LLZ-owned plane was
protected by mTLS**. Three tiers of exposure:

**Plaintext carrying credentials.**
- `harbor-robot-provisioner` → harbor-core over `http://`, sending the Harbor
  **admin password** in a Basic-auth header and receiving freshly minted **robot
  secrets** in the response.
- OpenBao → Keycloak JWKS over `http://…:8080` — a plaintext fetch of the
  **signing keys** used to validate team-login tokens. Substituting that response
  mints tokens OpenBao accepts; `bound_issuer` does not help, because the issuer
  claim is checked with the very keys being fetched.

**Plaintext, lower sensitivity.** Prometheus scrapes of the reconciler,
otel-collector, and cert-manager (`scheme: http`); the OTLP receiver on
`0.0.0.0:4318` with no TLS and no client auth.

**TLS with the server never authenticated.** Every pod→OpenBao call ran
`OPENBAO_SKIP_VERIFY=true` / `InsecureSkipVerify`. Encrypted, but neither end
proved anything.

The compensating controls were NetworkPolicy plus application-layer bearer
tokens. NetworkPolicy is an **authorization** control — it decides who may open a
connection. It provides no confidentiality and no peer authenticity, and it is
enforced by the CNI at the pod's veth, so it is bypassed by anything with
node-level access, `hostNetwork`, or a position in the CNI path. The controls and
the exposure were in different threat classes.

Two specific gaps made the authorization story weaker than it looked:
- `openbao-network-policy.yaml` and the reconciler's NetworkPolicy both use bare
  `- ports: [443, 6443]` rules **with no `to:` selector** — unrestricted egress to
  anything on :443, for the two workloads holding the most credentials.
- `harbor-allow-intra-namespace` is `podSelector: {}` both ways, so *any* pod in
  the `harbor` namespace could reach harbor-core — and `harbor` is apl-core's
  namespace, which LLZ does not label with restricted Pod Security.

## Decision

Adopt **certificate-based mutual TLS**, using cert-manager and the per-service CA
pattern this repo already runs (`openbao-bootstrap-ca`, `otel-bootstrap-ca`),
rather than a service mesh — except where the peer is not ours.

### Three trust roots, deliberately separate

| Root | Signs | Rationale |
|---|---|---|
| `openbao-ca` (existing) | OpenBao's serving cert | unchanged |
| `llz-client-ca` (new) | every workload's **client** identity | a client cert must never be usable as a server cert |
| `llz-serving-ca` (new) | LLZ services that terminate TLS themselves | `openbao-ca` means "this is OpenBao"; a reconciler cert signed by it would assert that falsely |

Roots are assertions. Sharing one across client and server identities means
"trusted to be a client" and "trusted to be the server" become the same
statement, and any leaf holder can impersonate the server to a peer that trusts
the root. All leaves carry `usages: [client auth]` or `[server auth]`, never both.

CA distribution uses the **ca.crt indirection**: issue a throwaway leaf from root
X into namespace N, and cert-manager writes X's certificate to `ca.crt` on the
resulting Secret. This platform ships no trust-manager/reflector, and ESO's
`caProvider` already sources `openbao-ca` this way — so it is the established
pattern, not a new one. If trust-manager is adopted later, each anchor
Certificate becomes a Bundle and nothing else changes.

### Relationship to #358

PR #358 landed the same indirection for the SERVER half while this change was in
flight: `openbao-ca-bundle` Certificates in llz-reconciler, harbor and
llz-pat-rotator, an `OPENBAO_CA_FILE` env, and a shared
`inClusterBaoHTTPClient()`. This change is **additive over it**, not a competing
design — those Secrets are reused as-is and the client half is layered on top.

Two things from #358 change here:

- **`OPENBAO_SKIP_VERIFY` is removed.** #358 kept it as a cold-start fallback for
  the window before the CA Secret exists. With a client certificate also
  required, unverified TLS cannot complete the handshake at all, so the fallback
  became unreachable. Leaving it would advertise a downgrade that no longer
  exists.
- **The client transport caches success only.** #358's `optional: true` posture on
  the reconciler's CA volume is kept (its many non-OpenBao lanes must not wait on
  a wave-0 Certificate), which means the CA can be briefly absent at cold start.
  A `sync.OnceValues` memo would cache that failure for the life of a process
  whose liveness probe never touches OpenBao — it would never recover. The client
  retries on failure and caches only once built.

### Per-hop outcomes

| Hop | Before | After |
|---|---|---|
| pod → OpenBao (reconciler, harbor-provisioner, pat-rotator) | TLS, verification off | **mTLS** |
| ESO → OpenBao | TLS, server verified | **mTLS** |
| OpenBao raft `retry_join` | TLS, server verified | **mTLS** |
| Prometheus → OpenBao `/v1/sys/metrics` | TLS, `insecureSkipVerify` | **mTLS** |
| Prometheus → reconciler `/metrics` | plaintext | **mTLS** |
| → OTLP receiver `:4318` | plaintext, no auth | **mTLS** |
| harbor-provisioner → harbor-core | plaintext, sidecar disabled | **mTLS via mesh** |
| OpenBao → Keycloak JWKS | plaintext | **TLS with pinned CA** (not mutual — see below) |
| kubelet → reconciler `/healthz` | plaintext | **plaintext, own port** (see below) |

### Two hops that are deliberately not mTLS

**Keycloak JWKS — one-way TLS.** Keycloak is not in the mesh and apl-core's
Keycloak.X does not do client-cert auth. The options were verified TLS, or an
apl-core change LLZ cannot make from this repo. Verified TLS closes the attack
that matters (response substitution); the residual gap is that Keycloak cannot
verify OpenBao, which for a public-key fetch of non-secret material is not a
meaningful exposure. `jwks_ca_pem` is the control — moving to `https` alone would
still accept any certificate.

**Reconciler `/healthz` — plaintext, separate port.** The client is the kubelet,
and an `httpGet` probe cannot present a client certificate. A single combined
listener requiring mTLS would fail every probe and CrashLoop the pod. The split
is safe because of what each endpoint returns: `/healthz` is a leader-election
verdict, `/metrics` is the gauge set. Combining them would have forced the weaker
posture onto both.

**OpenBao's loopback listener.** A second listener on `127.0.0.1:8210` serves the
in-pod operator paths (`bao operator init`, unseal, generate-root, `llz ci
health`) which run as the `bao` binary inside the container and hold no client
identity. It is reachable only from inside the pod's network namespace — no
NetworkPolicy applies because no packet leaves the pod — and every call on it
still requires a Vault token, so authorization is unchanged. `kubectl
port-forward` reaches it (forwarding is established inside the pod netns), which
is what keeps operator commands working without issuing certs to laptops. The
serving cert gained a `127.0.0.1` SAN so even these callers verify the server;
this removed the last `VAULT_SKIP_VERIFY` from the codebase.

### Harbor: mesh, not direct TLS

Direct `https://harbor-core:443` was considered and rejected: harbor-core has no
client-certificate auth, so it would be TLS, not mTLS. The mesh is the only
mechanism that makes that hop mutual. Consequences:

- the provisioner rejoins the mesh (`sidecar.istio.io/inject: "true"` plus
  `holdApplicationUntilProxyStarts`), reversing the previous opt-out;
- the never-exiting-sidecar problem that motivated the opt-out is handled by
  `shutdownIstioSidecar()` POSTing to `/quitquitquit` on exit — best-effort, and a
  no-op where istiod runs `ENABLE_NATIVE_SIDECARS=true`;
- a `PeerAuthentication` sets the harbor namespace to `STRICT`, which is what
  makes plaintext to harbor-core *impossible* rather than merely unused;
- the provisioner's NetworkPolicy gains istiod egress (15010/15012/15014) —
  without it Envoy's iptables redirect kills all pod egress.

This also resolves a standing contradiction: `ci_mesh_egress_guard.go` already
*asserts* harbor is STRICT and fails CI builds on that basis, while the
foundation chart says the mesh is PERMISSIVE. The provisioner working
sidecar-less over plaintext showed PERMISSIVE was the reality. The
PeerAuthentication makes the guard's assertion true.

### No downgrade switch

`OPENBAO_SKIP_VERIFY` is deleted, not defaulted off. Its failure mode was that
setting it silently disabled verification with no signal. Its replacement is a
mount: absent material is a hard, named error. `serveMetrics` likewise rejects a
*partial* TLS config rather than starting a server that encrypts but accepts any
caller.

## Consequences

**This is a fail-closed change.** `tls_require_and_verify_client_cert` rejects
certless clients at the handshake, before any token is read. Enabling it while a
consumer lacks a leaf takes that consumer down — and **ESO without a certificate
takes down every ExternalSecret in the cluster.** Hence the staged rollout below.

**New failure mode: certificate expiry.** Leaves are 90d/30d
(`duration`/`renewBefore`). A stalled cert-manager now breaks connectivity, not
just renewal. The `credential-inventory` dashboard already scrapes cert-manager
expiry metrics; alerting on the new leaves should be added before this reaches
production.

**Long-lived processes must re-read their keypair.** The reconciler is a
Deployment whose process outlives a 90-day leaf. A TLS client or server that
loads its keypair once at startup keeps presenting the ORIGINAL certificate after
cert-manager renews at day 60, then fails every handshake from day 90 — and
nothing recovers it, because the liveness probe reports leader-election health
and never touches OpenBao or the scrape path. Both directions therefore re-read
per handshake: `HTTPClientMTLSFromFiles` (`GetClientCertificate`) on the client
side and `serveMetrics` (`GetCertificate`) on the serving side.
`TestHTTPClientMTLSFromFiles_PicksUpRotation` is the regression test — it rotates
the files under a live client and asserts the change is observed. The CronJobs
are immune by construction (fresh process per run), which is exactly why this is
easy to miss.

**A cert reissue on deploy.** Adding the `127.0.0.1` SAN to `openbao-tls` makes
cert-manager reissue it, which the `openbao-cert-watcher` component observes and
responds to by restarting OpenBao — i.e. deploying this rolls the raft cluster.
Expected, but it is a production event, not a no-op config change.

**Not addressed here.** The bare `- ports: [443, 6443]` egress rules remain. They
are a real weakness in the containment story and should be scoped to explicit
`ipBlock`s, but that is an independent change and mixing it in would have made
this one harder to roll back.

### Accepted residual plaintext

Three hops stay in the clear. Each is a decision, not an oversight:

| Hop | Why it stays | Who could close it |
|---|---|---|
| Prometheus → cert-manager `:9402` | apl-core owns the deployment; LLZ cannot give it a serving cert. Payload is counters + expiry timestamps, no key material. | apl-core |
| Prometheus → otel-collector `:8888` | Service is operator-created; TLS only via the collector's thin `service.telemetry` support. Payload is `otelcol_*` internals. Breaking `OTelCollectorMetricsTargetDown` to encrypt a queue gauge is a bad trade. | upstream collector |
| kubelet → reconciler `/healthz` | `httpGet` probes cannot present a client cert; see above. Payload is a leader-election verdict. | nothing — inherent |

Separately, `openbaoPromtail.lokiPushUrl` points at `loki-gateway.llz-observability`
over HTTP, but **Loki runs in `monitoring`** (per the alert rules) and nothing
creates that Service — so OpenBao's audit log is almost certainly not reaching
Loki at all. That is a broken pipeline rather than an exposed one, and fixing it
is out of scope here; whoever repairs the URL must give it TLS at the same time.

### Known incompatibility

`llz reconcile --reconcile-harbor` runs the provisioner logic in-process from the
unmeshed reconciler pod, speaking plaintext to harbor-core. Once the harbor
`PeerAuthentication` is STRICT that lane cannot work. It is off by default and not
enabled in the Deployment. It is not made a hard error because meshing the
reconciler later would make it valid again; the failure is loud (`llz_reconcile_up=0`).
`ci_mesh_egress_guard.go` cannot catch this — it inspects NetworkPolicies, and
this path is Go.

`llz ci openbao-login` — both methods — now requires in-cluster mTLS material.
`--method oidc` was introduced as the fallback for an external GitHub-hosted
caller; that is no longer true, since an external runner has neither a client
certificate nor in-cluster DNS for the ClusterIP. External access goes through
`kubectl port-forward … :8210`. No workflow in this repo invokes either method
today, so this is a latent regression rather than a live break.

## Rollout

Fail-closed ordering — **each step must be verified before the next**:

1. Ship the CAs and **all** leaf Certificates. Enforcement is still off. Confirm
   every Certificate is Ready: `kubectl get certificate -A | grep -v True`.
2. Ship client-side config only (ESO `tls:` refs, Go client mounts, ServiceMonitor
   `tlsConfig`). Servers still accept plaintext/certless, so nothing breaks. Verify
   ESO stores stay Ready and `vault_*` / `llz_*` series continue.
3. Apply the harbor `PeerAuthentication` with `mode: PERMISSIVE`. Watch
   `istio_requests_total{connection_security_policy="none"}` for the namespace
   until zero — that is the list of clients that would break under STRICT. Then
   flip to `STRICT`.
4. Enable `tls_require_and_verify_client_cert` on the OpenBao listener. **This is
   the irreversible-feeling step**; have the `bao` loopback path ready
   (`kubectl port-forward … :8210`) before starting.
5. Re-run `llz ci bao-configure` so the Keycloak mount picks up the https
   `jwks_url` + `jwks_ca_pem`. Verify with a real `llz openbao login --team <x>` —
   the JWKS fetch is lazy, so a bad CA shows up only at first login.
6. Remove the `:8080` Keycloak allow from the OpenBao NetworkPolicy once every
   instance has completed step 5. Leaving it is a standing permission for the
   plaintext fetch this ADR exists to eliminate.

## The wiring is statically enforced — `mtls-wiring-guard`

This was found by mutation, not by review: **deleting the reconciler's
client-certificate volumeMount passed every gate in the repo** — `make lint-k8s`
reported zero errors, kustomize rendered, kubeconform was satisfied — while
leaving the pod unable to reach OpenBao at all. Green CI meant nothing here.

`llz ci mtls-wiring-guard` closes that, as a fifth member of the guard family
(`wave-health`, `wave-dependency`, `mesh-egress`, `plaintext`). It asserts three
things about every workload in the platform tree:

1. **A pod that declares `OPENBAO_ADDR` mounts what its code reads.** That pod
   will call `inClusterBaoHTTPClient()`, which opens a CA bundle and a client
   keypair, so paths covering all three must be mounted — honouring the
   `OPENBAO_CA_FILE` / `OPENBAO_CLIENT_{CERT,KEY}_FILE` overrides where set.
2. **Every TLS Secret it mounts has a Certificate creating it**, in the same
   namespace. A rename on either side of that pair is otherwise invisible until
   a pod sits in `ContainerCreating` on a real cluster.
3. **`OPENBAO_SKIP_VERIFY` does not come back.** A mounts-only check would pass a
   pod that had both the mounts and the escape hatch.

The requirement is **inferred, not registered**. There is no allowlist to
maintain and no way to add an OpenBao consumer that silently escapes the rule —
declaring the address is what opts you in. That is the property a registry-based
version would lose, and it is why this guard has no counterpart to
`plaintextAllowed`.

Both mutations above now fail the guard, and both are pinned as tests
(`TestMTLSWiringCatchesMissingClientCert`, `TestMTLSWiringRejectsSkipVerify`)
alongside the CronJob-nesting case — two of the three consumers are CronJobs, and
a guard that only understood Deployments would have skipped them silently.

**What it still does not cover:** the guard reads manifests, so it cannot see
whether the Secret contents are valid, whether cert-manager actually issued, or
whether the far end accepts the certificate. Those need a cluster.

## Unverified prerequisites

These could not be confirmed from the repo and **must be checked on a live
cluster** before rollout:

1. **Keycloak serves TLS on 8443** with a cert from apl-core's CA:
   ```
   kubectl -n keycloak get svc keycloak-keycloakx-http -o yaml | grep -A4 ports
   ```
   If it does not, set `platform.aplCA.enabled=false` and pin static keys via
   `jwt_validation_pubkeys` instead.
2. **apl-core exposes a `custom-ca` ClusterIssuer** (`kubectl get clusterissuer
   custom-ca`). The only evidence in-repo is a by-name reference in
   `otel-bootstrap-ca.yaml`'s migration note.
3. **The harbor namespace has no unmeshed plaintext clients** beyond the CNPG
   (:8000) and metrics (:8001) ports already exempted — step 3 above measures this.
4. **`prometheus-operator` resolves ServiceMonitor `tlsConfig` secret refs in the
   ServiceMonitor's own namespace.** This is its documented behavior and all
   scrape Secrets are placed accordingly, but it has not been observed here.
5. **ESO's Vault provider supports `spec.provider.vault.tls.{certSecretRef,keySecretRef}`**
   on the apl-core 6.x ESO build. The CRD is `external-secrets.io/v1`, where the
   field exists; confirm against the live CRD:
   `kubectl get crd clustersecretstores.external-secrets.io -o yaml | grep -A5 'tls:'`
