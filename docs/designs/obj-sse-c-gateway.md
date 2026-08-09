# Design: SSE-C gateway for Object Storage encryption at rest

**Status:** Partial — built, not deployed. `spec.components.objProxy` is default-disabled and
the DNS rewrite that activates it is deliberately outside the kustomization.

## Problem

Loki chunks (every log line, including the OpenBao audit stream) and Harbor image
layers live in Linode Object Storage. Nothing encrypts them.

The obvious fixes are both dead, and they were **measured** rather than reasoned
about — this whole design exists because "it is S3-compatible, so the header must
work" turned out to be false. Probed 2026-07-31 against a scratch bucket on
`us-ord-10` (E3), with a temporary scoped key, all of it deleted afterwards:

| Request | Result |
|---|---|
| plain `PUT` then `HEAD` | `200`, **no** `x-amz-server-side-encryption` — nothing applied by default |
| `x-amz-server-side-encryption: AES256` (SSE-S3) | **`400 InvalidArgument`** |
| SSE-C (customer-provided key) | `200`; keyless `HEAD` → `400`. Works. |
| `PutBucketEncryption` / `GetBucketEncryption` | **`501 NotImplemented`** |

SSE-S3 is not merely unsupported, it is **rejected**. Harbor's registry
(`encrypt: true`) and Loki (`sse.type: SSE-S3`) can each request it in one line of
values, and on Linode that returns 400 on every blob push and every chunk flush —
it breaks the writer rather than degrading to plaintext. `PutBucketEncryption`
being 501 removes the app-agnostic lever too.

**SSE-C is the only mode Linode implements, and neither writer can emit it.** Loki's
`SSEConfig` accepts only `SSE-KMS` and `SSE-S3` and hard-errors otherwise;
distribution's S3 driver exposes only `encrypt` and `keyid`. Reaching SSE-C means
forking both.

### Why not move the data to encrypted block storage instead

For **Harbor**, that would work — it ran on a 100Gi PVC before, and
`block-storage-retain` is encrypted and default.

For **Loki**, it is not a performance trade, it is an **architecture break**:

> "Running Loki clustered is not possible with the filesystem store unless the
> filesystem is shared in some fashion (NFS for example)."

The deployment runs three ingesters behind separate queriers. On `type: filesystem`
each ingester writes chunks to its own local disk and the querier cannot read them —
**queries return silently incomplete results**, which is the worst possible failure
for a log store and especially for an audit trail. `loki-ingester`'s `/var/loki` is
an `emptyDir` today, so chunks would also be lost on restart.

And the switch is shared: apl-core keys both writers off the same
`$obj.provider.type`, so there is no "Harbor on a PVC, Loki on OBJ" configuration.
Either both move — breaking Loki — or neither does.

## Goals / non-goals

**Goals.** Objects in the Loki and Harbor buckets are encrypted at rest with a key
Linode does not hold. No fork of Loki or distribution. No new cryptography written
here. Verifiable at runtime, not merely configured.

**Non-goals.** Defending against compromise of the cluster (the key lives in it).
Encrypting objects written before the gateway went live. Replacing the buckets.

## Design

A reverse proxy that **injects SSE-C headers**. It owns no cryptography — Linode
does the encryption; object bodies pass through untouched.

```
Loki / Harbor ──https──▶ obj-proxy ──https──▶ us-ord-N.linodeobjects.com
  (unchanged)          (adds SSE-C headers)         (encrypts)
```

Three measured facts make this small enough to trust on the write path:

1. **SSE-C headers are honoured outside SigV4 `SignedHeaders`.** So on the normal
   path the proxy does **not re-sign** — it changes neither body, method, path nor
   Host, and the client's signature stays valid. This is blind header injection,
   not termination. It is also why callers must reach the proxy at the **real
   endpoint hostname**: rewriting Host would invalidate the signature we
   deliberately do not recompute.

   **One exception, added later: the #397 repair.** Linode's Ceph rejects the AWS
   SDK's default `PutObject` framing (`content-encoding: aws-chunked` with an
   `x-amz-trailer` CRC32), and Loki cannot be configured out of sending it. Those
   requests — and only those — are de-chunked and re-signed, because the framing
   headers are inside `SignedHeaders` and cannot be removed without it. The
   property above still holds for every other request. See
   `tools/cmd/llz/objproxy_resign.go`; the capability is off unless `--creds-file`
   is given, applies only to that framing, and re-signs as the **same access key
   the client used**, refusing any other. It is the one place the proxy holds a
   credential, which is a real increase in what a compromised proxy could do and
   the reason it is opt-in rather than always on.
2. **Blanket injection survives multipart**, including `CompleteMultipartUpload`.
   It assumes **path-style** addressing throughout: the object key is read out of
   `/<bucket>/<key>`. Both writers use it (Loki pins `s3forcepathstyle: true`, and
   Harbor's encrypted blob on the e2e cluster proves it resolves the plain endpoint
   name). A virtual-host-style request is refused rather than forwarded — see
   Failure modes.
3. **Server-side COPY needs more.** Copying an encrypted source additionally
   requires the `x-amz-copy-source-server-side-encryption-customer-*` trio;
   destination headers alone fail `400 InvalidArgument`. Harbor reaches this path —
   apl-core sets `multipartcopythresholdsize: 5GiB`, so blobs above that use
   server-side copy. Rare, not impossible, and it fails closed.

SSE-C also makes the key **customer-managed** — Linode discards it on receipt. Read
that as a **cost, not a feature**. The control being satisfied here is *encryption
at rest*; customer-managed keys are not required, so holding the only copy of the
key buys nothing against the requirement while adding the sharpest failure mode in
this design (see Failure modes). It is an unavoidable side effect of the only
mechanism Linode implements, not a benefit worth preserving.

That is the single most important thing to know about this component: **it is a
bridge, not a destination.** Provider-managed SSE satisfies the requirement just as
well and carries none of the key risk, so the day Linode ships it this design should
be retired rather than kept.

### Addressing: why DNS

apl-core renders the endpoint for both writers as
`https://{{ $obj.linode.region }}.linodeobjects.com`, derived from `region` and not
independently settable. DNS is the only layer that redirects the traffic without
altering what the client signed.

The Corefile is Flux-managed (`kube-system/workload-coredns`), so patching it would
be reverted. It ships CoreDNS's standard hooks — `import custom/*.include` inside
the `.:53` block, backed by an **optional** `kube-system/coredns-custom` ConfigMap —
so the rewrite is a supported extension, not a patch.

The proxy must not see its own rewrite: `dnsPolicy: Default` makes it resolve via
the node's resolver.

Loop protection is a MARKER HEADER, not the startup check. Outbound requests carry
`X-Llz-Obj-Proxy`, and an inbound request already carrying it is refused
`508 Loop Detected`. The startup check compares the resolved upstream against the
pod's own addresses — which misses the likely shape entirely, because the rewrite
points at the obj-proxy **Service** and a ClusterIP is not a pod address, so the
traffic loops through kube-proxy possibly hitting a different pod each hop with no
process ever seeing itself. Alert on `llz_objproxy_loops_detected_total`: the
requests fail closed, but every one is a write that did not happen.

### Trust: why only Harbor needs work

Measured on the live cluster, the two writers are **not** symmetric:

- **Loki needs nothing.** Its rendered config carries
  `common.storage.s3.http_config.insecure_skip_verify: true` — it does not verify
  the S3 server certificate and accepts the proxy's cert as-is.
- **Harbor is the whole blocker.** `secure: true`, no skipverify, and the registry
  Deployment mounts **no CA material at all**. There is no `ca-bundle`/`custom-ca`
  ConfigMap anywhere in the cluster: apl-core exposes no custom-CA injection point
  on managed.

> That Loki flag is apl-core's and it is a real pre-existing TLS-verification gap on
> the hop carrying object-storage credentials and every log line. It happens to
> remove a blocker here; it is worth raising upstream on its own merits, and
> `plaintext-guard` cannot see it because it scans what *this repo* ships rather
> than what apl-core renders into the cluster.

So Harbor's CA arrives by **Kyverno mutation on the Pod**, not the Deployment.
Mutating the Deployment would make the live object differ permanently from the Helm
release and Flux compares Deployments — a drift-and-correct loop, the same class as
the two-default-StorageClass wedge. Pods are not compared. This is how Istio injects
its own sidecar into these same pods.

Delivery is `SSL_CERT_DIR`, not overwriting the bundle. Go's `crypto/x509` builds
the pool from a **file** list and a **directory** list, combined; `SSL_CERT_DIR`
replaces only the directories, so the image's own bundle still loads and no public
trust is lost. Measured inside the running container:

```
/etc/pki/tls/certs/ca-bundle.crt   887587 bytes   (Go certFiles[1] — Photon)
/etc/ssl/certs                     334 entries    (Go certDirectories[0])
/etc/ssl/certs/ca-certificates.crt ABSENT         (Go certFiles[0])
```

The CA Secret is issued **into the `harbor` namespace** — a Pod can only mount
Secrets from its own namespace, so it cannot live beside the proxy.

## Components

| Path | What |
|---|---|
| `tools/cmd/llz/objproxy_inject.go` | injection rules (pure, unit-tested) |
| `tools/cmd/llz/objproxy.go` | `llz obj-proxy` — TLS in, streamed proxy out |
| `platform-apl/components/objProxy/` | DaemonSet, Service, Certificate, ExternalSecret, NetworkPolicy |
| `.../obj-proxy/coredns-rewrite.yaml` | the on switch — **not** in the kustomization |
| `.../obj-proxy/ca-trust.yaml` | CA bundle, issued into `harbor` |
| `.../obj-proxy/kyverno-harbor-ca.yaml` | Pod mutation — in the COMPONENT, see below |
| `tools/cmd/llz/ci_seed_ssec_key.go` | `llz ci seed-ssec-key` — generate-once |
| `tools/cmd/llz/ci_assert_obj_encryption.go` | `llz ci assert-obj-encryption` |

The Kyverno policy lives in the **component**, not in `tools/internal/extensions/bootstrapcluster/manifests/`
with its siblings. Those are applied by `llz ci apply-kyverno-policy`, and that
command is invoked by nothing — no workflow, no Terraform, and `bootstrap-cluster`
does not call it despite `ci.go` saying it does. Putting it there would have shipped
a ClusterPolicy that never reaches a cluster, which for a CA-delivery mechanism
means Harbor silently loses object storage the moment the rewrite goes live. (The
same orphaning applies to the four policies still in that directory, including
`pvc-force-encrypted-storage-class` — out of scope here, but worth knowing.)

A **DaemonSet** rather than a Deployment: this is on the path of every image pull
and log write, so a rolling Deployment's brief unavailability is a cluster-wide
stall. Per-node keeps failures scoped to one node's workloads, and
`internalTrafficPolicy: Local` avoids a cross-node hop for large layers.

Wave **5**, not negative. The instinct is to start it early because it is on the
write path — but it cannot start without the key ESO serves from OpenBao at wave 0,
so a negative wave would block the bootstrap from reaching the OpenBao that would
unblock it. `wave-health-guard` catches this.

## Rollout (migration IN)

There is currently **nothing to migrate**: all three Harbor buckets are empty, and
only two Loki chunk buckets hold anything. So the risk is entirely **ordering** — if
the proxy is not live before Loki's first chunk flush, a fresh deployment gets a
mixed bucket for no reason.

1. Enable `spec.components.objProxy`; render; let it sync.
2. `llz ci seed-ssec-key --region <env>`. **Escrow the printed key offline.**
3. Confirm the DaemonSet is Ready on every node.
4. Confirm the Kyverno mutation landed: restart `deploy/harbor-registry` and check a
   running pod carries `SSL_CERT_DIR` and the CA volume.
5. **Then** apply `coredns-rewrite.yaml`. This is the cutover.
6. `llz ci assert-obj-encryption --endpoint <host> --bucket <bucket>`.

Flipping step 5 before step 4 turns an unencrypted-but-working registry into a
broken one.

For the two buckets that already hold plaintext chunks, **do not write a rewrite
job**: Loki chunks have a retention window, so everything predating the cutover ages
out on its own. One retention period later the bucket is uniformly encrypted. The
honest audit answer during that window is "encrypted from date X, plaintext aging
out through X+retention".

> **But the INDEX does not age out gracefully, and this was measured the hard way.**
> Loki's index-gateway enumerates a table's files and reads *every* one. It does not
> skip an object it cannot decrypt — it fails the whole table and retries:
>
> ```
> GetObject 400 InvalidArgument: The calculated MD5 hash of the key
>                                did not match the hash that was provided
> ```
>
> Queries then degrade (measured at ~33s against a healthy cluster, enough to time
> out clients) until the unreadable index files age out. So the drain window is not
> "correct but partially plaintext" — it is **correct but slow**, and slow enough to
> break callers.
>
> Two consequences worth planning around:
>
> - **Migrating IN over a populated bucket** costs a degraded query path for one
>   retention period, not zero. If that is unacceptable, empty the index prefix at
>   cutover rather than letting it drain.
> - **A NEW KEY over an existing bucket is not a migration at all.** SSE-C is
>   per-object, so a fresh key makes every existing object permanently unreadable.
>   That is what e2e hit: each cluster mints its own key while reusing the bucket, so
>   teardown now destroys the data buckets as well as the cluster
>   (`release-e2e-lane.yml`). Anywhere else, a new key over old data means those
>   objects are gone — treat it as key loss, because it is.

## Verification

`llz ci assert-obj-encryption` runs four checks, and the last two are the point:

1. **Pod** — registry pods carry the CA. Read from the *running pod*, never the
   policy: the live Kyverno webhook is `failurePolicy: Ignore`, so a pod admitted
   while Kyverno was down silently has neither the env nor the volume.
2. **DNS** — the rewrite is actually in `coredns-custom`. Catches the failure that
   looks exactly like success: every component Healthy, every byte going direct.
3. **Object** — sampled objects answer `400` to a **keyless** `HEAD`.

Check 3 needs no key: an SSE-C object cannot be read without one, so `400` proves
encryption and `200` proves plaintext. The gate never holds the unrecoverable
secret. `--bucket` is required and an empty bucket **fails** — checks 1 and 2 both
pass on a cluster that has never encrypted anything.

Sampling proves failure, not success. The output reports "0 of 50 sampled objects
were plaintext" rather than "encrypted", because the sample size is the auditable
number.

## Certificate rotation

The proxy re-reads its serving keypair **per handshake**. This is load-bearing, not
tidiness: the Certificate is 90d with `renewBefore` 30d, so cert-manager rotates the
Secret at day 60, and a server that loaded at boot would keep presenting the
original leaf until it expired at day 90 and then fail every handshake — a
cluster-wide outage on a timer, with the pod still Ready because liveness is on the
separate plaintext health port.

## Failure modes

| Failure | Behaviour |
|---|---|
| Proxy down (with rewrite live) | Writes and pulls fail. **Fails closed** — no silent plaintext |
| Kyverno down at registry admission | Pod lacks the CA; S3 fails loudly; **self-heals on next restart** |
| Rewrite never applied | Everything Healthy, everything plaintext. Only check 2 sees it |
| Key lost | **Every object unrecoverable.** Linode keeps no copy |
| DNS routes the endpoint back to the proxy | `508` per request, counted; fails closed, no silent plaintext |
| Re-signing credential missing/unreadable | Proxy refuses to start. Loud, not a silent loss of the #397 repair |
| Re-signing credential ROTATED | Re-read from the mounted Secret on mtime change — no restart needed. Reading it once would 403 every repaired write after `rotate-linode-creds` until each pod happened to restart |
| Pre-existing `kube-system/coredns-custom` | Argo takes ownership and REPLACES it, losing other keys. Absent on apl-core, so no current cluster is affected; an adopter with one must fold their keys into the component file |
| Virtual-host-style request (`bucket.<endpoint>/<key>`) | **Refused, 421.** The key is not in the path, so it cannot be encrypted correctly here. Nothing routes this way today — the rewrite is exact-match — but broadening that rewrite would otherwise arm a silent-plaintext path |
| A repair fails mid-request | Original forwarded untouched; upstream's own answer stands. Body is buffered first precisely so this cannot truncate a write |
| Payload over the 32MiB repair cap | Forwarded unrepaired — fails upstream as it does today. Bounded on purpose: the proxy is a DaemonSet and an unbounded buffer would take object storage down for the whole node |

The proxy also holds an object-storage **key** now, for the #397 repair
(`--creds-file`). It is the same key the clients behind it already use, so it grants
no new reach over object storage — but a compromised proxy could *mint* requests
rather than only relay signed ones, which is why the capability is opt-in and
refuses to re-sign under any access key but the caller's own.

Key loss is the sharpest. It joins `OPENBAO_SEAL_KEY` and
`TF_STATE_ENCRYPTION_PASSPHRASE` in the "lose it and the data is gone" class, and
being newest it is the one a DR rehearsal is most likely to miss.

Rotation is **not** a flag flip: SSE-C keys are per-object, so rotating means
rewriting every object under the new key. Budget it against bucket size.

---

# Retiring this when Linode ships managed SSE (expected Q4)

This design is a workaround for a missing platform feature. When Linode ships
managed SSE it should be **removed**, not kept — it is a component on the write path
guarding an unrecoverable key.

**Retirement is unconditional.** The control being satisfied is *encryption at
rest*, not customer-managed keys, so provider-managed SSE-S3 is a complete
replacement. There is no scenario short of the feature not shipping in which this
gateway earns its keep — every property it has beyond at-rest encryption is a cost.
Plan the removal as part of adopting the feature, not as a later cleanup that never
gets scheduled.

## The trap

The obvious sequence — enable managed SSE, delete the proxy — **destroys data
access**. Every object written through the gateway is encrypted with a key Linode
does not hold, so once the proxy stops injecting, a keyless `GET` returns `400`.
Existing objects become unreadable the moment the proxy goes away.

Retirement is therefore the migration-in problem run backwards, and it has the same
shape: **the old and new schemes must overlap while the old objects drain.**

## Step 0: verify the feature, do not trust the announcement

Re-run the probe from the Problem section. Managed SSE is real only when:

- `PUT` with `x-amz-server-side-encryption: AES256` returns **200** (today: 400)
- `PutBucketEncryption` returns **200** (today: 501)
- a plain `PUT` into a default-encrypted bucket comes back with the
  `x-amz-server-side-encryption` header on `HEAD`

This entire document exists because that inference was made once and was wrong. Do
not skip it. The scratch-bucket probe is minutes of work.

## Step 1: drain mode — the one code change retirement needs

`injectSSEC` currently injects on every object GET/HEAD/PUT/POST. Retirement needs a
mode that **injects on reads but not on writes**:

- reads still carry the SSE-C trio, so legacy objects stay readable
- writes carry nothing, so they land under the bucket's managed-SSE default

That is a flag on `llz obj-proxy` (`--drain`) and a branch in `injectSSEC` keyed on
the HTTP method — roughly ten lines, plus tests. It is not implemented today, and it
is the difference between "retirement needs a bespoke rewrite job" and "retirement
is a flag flip and waiting".

## Step 2: retirement sequence

1. **Enable bucket default encryption** (`PutBucketEncryption`, SSE-S3) on each
   bucket. New writes are managed-SSE from here.
2. **Flip the proxy to `--drain`.** New objects → managed SSE. Old objects → still
   readable, because reads still carry the key. Both schemes coexist; nothing
   breaks.
3. **Drain the legacy objects.** This is where the two consumers differ:
   - **Loki drains itself.** Wait one full retention period. No code, no job.
   - **Harbor does not.** Image layers persist indefinitely. Either re-push (simple,
     disruptive) or rewrite in place — a self-copy carrying the copy-source SSE-C
     trio and *no* destination SSE-C headers, so it lands under the bucket default.
     A self-copy that changes encryption attributes is permitted where a
     metadata-identical one is not; **verify that against Linode before relying on
     it**, and fall back to copy-to-temp-key-and-back if it is refused.
4. **Verify nothing still needs the key.** Extend the gate with an
   `--expect managed` mode that inverts check 3: a keyless `HEAD` should now return
   `200` *and* report `x-amz-server-side-encryption`. Sample hard here — the
   sampling caveat applies with full force, because a missed object is one that
   becomes permanently unreadable at step 5.
5. **Remove the DNS rewrite.** Traffic goes direct. This is the actual cutover:
   instant, and reversible by re-applying the ConfigMap.
6. **Remove the rest** — Kyverno policy, CA Certificates, DaemonSet, ExternalSecret,
   the `objProxy` component, and the `llz obj-proxy` / `seed-ssec-key` commands.
7. **Do not destroy the key yet.** Keep it escrowed for a defined window (a quarter
   is reasonable) after step 5. If the drain missed anything, those objects are
   recoverable *only* with it, and step 4's sample cannot prove full coverage.
   Destroy it after the window, deliberately, as its own change.

## Step 3: retire the scaffolding around it

- Delete the four `linode_object_storage_bucket` entries from `atRestAllowed` and
  add the bucket-level encryption lever to `atRestResourceLevers` — buckets stop
  being a registered residual and become a checked one.
- Update ADR 0007 (state encryption): the Context table's measured numbers become historical, and
  "Backend SSE (`encrypt = true`)" moves from *rejected* to *adopted*.
- Retire the `ohttp`/`lab` E1 caveat if managed SSE lands only on E2/E3.

## What "done" looks like

No proxy on the write path, no Kyverno mutation on an apl-core Deployment, no DNS
rewrite, no unrecoverable key — and the at-rest guard checking a bucket-level
argument the same way it checks `disk_encryption` today. The audit answer becomes a
provider attestation rather than a system we operate.

## Open questions

- ~~Will managed SSE support customer-managed keys?~~ **RESOLVED — it does not
  matter.** The control is *encryption at rest*; provider-managed keys satisfy it.
  So plain SSE-S3 is a complete replacement for this gateway, and the retirement
  plan above runs unconditionally once the feature ships. This was the one open
  question that could have made the gateway permanent; it is closed.
- Which endpoint generations get it? `ohttp` and `lab` are on E1, and a
  managed-SSE rollout limited to E2/E3 would leave those two deployments on this
  gateway after the others have retired. Check before assuming a fleet-wide cutover.
- Does Linode's implementation re-encrypt existing objects on
  `PutBucketEncryption`, or only new writes? Every major implementation is
  new-writes-only, but step 3 depends on it and it is one probe to confirm.
