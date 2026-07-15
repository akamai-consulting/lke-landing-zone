# Design: narrow in-cluster Linode PAT + DNS-token consolidation (rotator tier-2)

**Status:** IMPLEMENTED (Phase A landed with the #136 stack; Phase B landed on
top of it — see the implementation notes below). Gated on a green Release-E2E
per §7 before promotion past lab.

> **Implementation notes (Phase B, as landed).** The open questions resolved:
>
> - **§4 (apl-core existingSecret) — spike result: NOT supported on v6.0.0**
>   (`dns.provider.linode` requires an inline `apiToken` string;
>   `additionalProperties: false`). But the spike found a cleaner seam than
>   option (c)'s Secret fight: apl-core v6 feeds BOTH DNS consumers through
>   **ExternalSecrets** it manages (`external-dns` in ns cert-manager for the
>   webhook, `linode-dns-api-token` in ns external-dns for ExternalDNS; source
>   `core-secrets-store`/`dns-secrets`, property `provider_linode_apiToken`).
>   The landed shape mutates those two **ExternalSecrets at admission**
>   (`kyverno-dns-rotating-token.yaml` + a mutate-existing catch-up), pointing
>   `secretStoreRef`/`remoteRef` at the `openbao` store's
>   `linode/api-token`/`token`. Deterministic per apply → no ownership flap;
>   target templates untouched. `TF_VAR_linode_dns_token` stays as the
>   schema-required first-boot fallback (used only until the policy syncs).
> - **§5 (scoped-PAT self-mint) — sidestepped: outcome B.** Rotation stays in
>   CI; the broad PAT mints the narrow one. Better still, each region's job
>   mints its OWN token (`llz ci rotate-incluster-pat`, label
>   `llz-incluster-<region>`) — no GitHub-secret hop, no cross-job value.
> - **§3.1 path — repurposed `secret/linode/api-token`** (not a new
>   `incluster-pat` path): every existing consumer (volume-labeler ES, the
>   rotator's minting cred, #137's cidr-firewall ES) and the
>   `secret-propagator`/`platform-ci` policies keep working unchanged.
> - **Scopes grew for #137** (cidr-firewall self-discovery): the narrow PAT is
>   `domains:rw object_storage:rw volumes:rw` **+ `linodes:ro vpcs:ro
>   firewall:rw`** — still nothing Terraform-shaped (no lke/vpc:rw/
>   nodebalancers/account).
> - `llz ci mint-bootstrap-pat` seeds the first token at bootstrap
>   (skip-if-present, rotated_at-stamped); `llz ci bao-seed-all` no longer
>   seeds the path; `llz ci propagate-pat` is retired.
**Item:** resolves **tier-2** of
[in-cluster Linode credential rotator](linode-credential-rotator.md) (the
dual-domain `LINODE_API_TOKEN`), using the "cert-manager reads the narrow PAT's
domains capability directly; drop the separate DNS sub-token" option.
**Relates to:** `platform-apl/components/linodeCredRotator/` (the rotator),
`platform-apl/components/volumeLabeler/` (the other in-cluster PAT consumer),
`platform-apl/manifest/dns/` (the DNS-01 wiring),
`llz ci mint-bootstrap-objkeys` / `rotate-linode-creds` / `propagate-pat`,
`.github/workflows/llz-secret-rotation.yml`.

> This is a design PR (no code). It exists because the "obvious" version of the
> change — *point cert-manager at a rotating token and delete the DNS
> sub-token* — runs into two non-obvious realities in the current wiring that
> need a decision before any code lands. Both are documented below.

---

## 1. The current DNS-token reality (two paths, one of them vestigial)

There are **two** Linode DNS tokens in play today, and the one the rotator
rotates is **not** the one that solves ACME challenges.

**Path A — the real solver (static, never rotates).**
`apl-values/<env>/values.yaml` sets `dns.provider.linode.apiToken:
${linode_dns_token}`, a plain **string** rendered by the cluster-bootstrap
`templatefile()` from `TF_VAR_linode_dns_token` at apply time. apl-core feeds
that string to **ExternalDNS** and to **`cert-manager-webhook-linode`** — the
canonical DNS-01 solver (`groupName: acme.slicen.me`,
`letsencrypt-clusterissuer.yaml`). This token is static in TF/values and is
**never rotated in-cluster**.

**Path B — `secret/certmanager/dns01` (rotated, but vestigial).** OpenBao
`secret/certmanager/dns01` → ESO → the `cert-manager-dns01-solver-token`
Secret. It is:
- **rotated** by the rotator (`dns-token` entry, `domains:read_write`, in
  `buildRotationTable`),
- **seeded** at bootstrap (`bao-seed-all`, from `LINODE_DNS_TOKEN`),
- read by **nothing that solves DNS-01** — its only remaining consumer is the
  `llz ci wait` bootstrap gate. The issuer file says so outright: *"it is no
  longer read by a landing-zone webhook."*

**Consequence:** the rotator is rotating a dead token while the token that
actually authorizes DNS-01 (and ExternalDNS) is static and unrotated — an
InfoSec gap hiding behind machinery that *looks* like it handles DNS rotation.
Any honest "consolidate the DNS token" work has to fix *Path A*, not just tidy
Path B.

---

## 2. The tier-2 problem this rides on

The [rotator design](linode-credential-rotator.md) left `LINODE_API_TOKEN` as
tier-2: a **broad** provisioning PAT (`llz-secret-rotation.yml` mints it with
`linodes:rw object_storage:rw lke:rw firewall:rw vpc:rw volumes:rw
nodebalancers:rw events:read account:read_write`) that is **dual-domain**:

- **Out-of-cluster:** Terraform/CI use it for everything (cluster, VPC,
  firewall, object-storage buckets, …). It is rotated by CI
  (`secret-rotation.yml` → `llz ci propagate-pat` → each region's OpenBao).
- **In-cluster:** ESO syncs it to `secret/linode/api-token`, read by the
  **volume-labeler** (needs `volumes:rw`) and used as the **minting
  credential** by the rotator / `mint-bootstrap-objkeys` / `temp-objkey`
  (needs `object_storage:rw`, and `account:rw` to mint sub-tokens).

`secret-rotation.yml`'s own header calls this out: *"the guidelines recommend a
Kubernetes-scoped PAT; reusing the Terraform token instead is a deliberate
accepted deviation."* Closing that deviation is the goal.

---

## 3. Proposal

Introduce a **narrow, in-cluster-only Linode PAT** and make it the single token
every in-cluster Linode consumer reads — including DNS-01. Drop the separate
DNS sub-token entirely.

### 3.1 The narrow PAT

- **Scopes:** `object_storage:read_write volumes:read_write
  domains:read_write` — the union of what in-cluster workloads need, and
  nothing else. No `linodes`/`lke`/`firewall`/`vpc`/`nodebalancers` (Terraform's
  concerns), no `account:read_write` unless self-rotation forces it (§5).
- **OpenBao path:** `secret/linode/incluster-pat` (new; keeps the broad
  `secret/linode/api-token` name free of the meaning change, or repurpose that
  path — decide at implementation).
- **Consumers (all read the SAME rotating Secret via ESO):**
  - volume-labeler — `volumes:rw`;
  - the rotator + `mint-bootstrap-objkeys` + `temp-objkey` — `object_storage:rw`;
  - **DNS-01 solver + ExternalDNS** — `domains:rw` (this is the consolidation).
- **Least-privilege trade-off (accepted, per the chosen option):** the DNS
  solver and ExternalDNS end up holding a token that also carries
  `object_storage`/`volumes`, broader than domains-only. We accept that in
  exchange for **one** rotating token instead of a separate domains-only token
  with its own mint/rotate/seed/ExternalSecret machinery. The rejected
  alternative (a dedicated domains-only rotating token) is strictly more moving
  parts for a marginal scope reduction on an already-in-cluster credential.

### 3.2 Bootstrap + rotation

- **Bootstrap mint:** `llz ci mint-bootstrap-objkeys` grows a sibling (or a new
  `mint-bootstrap-pat`) that mints the narrow PAT once and seeds
  `secret/linode/incluster-pat`, `rotated_at`-stamped + skip-if-present — the
  exact pattern #129 established for the object-storage keys. The **broad**
  provisioning PAT (which has `account:rw`) does the minting at bootstrap.
- **Rotation:** a rotation-table entry for the narrow PAT (PAT kind → the
  existing verify-via-`GET /v4/profile` path). See §5 for who is allowed to
  mint the replacement.

### 3.3 What the broad PAT becomes

CI/Terraform-only. It is **no longer ESO-synced into any cluster**
(`secret/linode/api-token`'s in-cluster consumers all move to the narrow PAT).
It keeps its CI rotation in `secret-rotation.yml`, but `llz ci propagate-pat`
(which pushes it *into* clusters' OpenBao) is deleted.

---

## 4. Open question #1 (gating) — feeding apl-core a *rotating* DNS token

This is the crux, and it is apl-core-shaped.

apl-core takes the DNS token as a **values string**
(`dns.provider.linode.apiToken`), not as an existing-Secret reference — the
cluster-bootstrap variable description even notes the placeholder "satisfies
apl-core's string-type schema requirement." A string baked into values.yaml
**cannot** be a rotating, ESO-synced credential. apl-core's ExternalDNS and its
`cert-manager-webhook-linode` both consume that string (via a Secret apl-core
creates from it). So "point cert-manager at the narrow PAT" requires getting
apl-core's DNS consumers to read an **ESO-owned** Secret instead of an
apl-core-owned one. Options, roughly in order of preference:

- **(a) apl-core existingSecret support.** If the apl-core version in use lets
  `dns.provider.linode` reference an existing Secret (name/key) rather than an
  inline token, point it at the ESO-synced narrow-PAT Secret. Cleanest; needs
  a schema check against the pinned `apl_chart_version`.
- **(c) Kyverno mutation / post-render.** A mutation that rewrites the
  apl-core-generated webhook + ExternalDNS Secret references (or the Secret
  contents) to the ESO-synced one. The repo already uses Kyverno for exactly
  this class of apl-core-gap patch (`_shared/manifest/kyverno-policies/`), so
  it is idiomatic — but it is a genuine ownership fight with apl-operator over
  a Secret and must be proven not to flap.
- **(b) Landing-zone-owned webhook.** Ship our own
  `cert-manager-webhook-linode` pointed at the ESO Secret. Rejected by the
  current design (*"apl-core's is canonical; the landing zone does NOT ship its
  own webhook"*) — reintroduces a divergence we deliberately removed. Listed
  for completeness.
- **(d) Do nothing for apl-core; only fix ExternalDNS if it's separable.**
  Fallback that leaves the DNS-01 webhook static — does **not** meet the goal;
  documented only as the floor.

**Recommendation:** spike (a) against the pinned apl-core version first; fall
back to (c). If neither is viable on the current apl-core, the DNS half of this
design is **blocked on apl-core** and only Phase A (below) should land.

---

## 5. Open question #2 — can the narrow PAT self-rotate?

Rotating a PAT means calling `POST /v4/profile/tokens`. The **broad** PAT can
because it carries `account:read_write`. It is **unconfirmed** whether a scoped
PAT *without* `account:read_write` may create sub-tokens at all, or only
sub-tokens within its own scopes. Two outcomes:

- **If a scoped PAT can self-mint** (subset of its own scopes): the rotator
  owns the narrow PAT end-to-end, in-cluster, no CI. Best outcome; but note
  self-minting means the narrow PAT effectively holds token-creation power,
  which erodes the "narrow" claim.
- **If token creation requires `account:read_write`:** keep narrow-PAT rotation
  **out** of the in-cluster rotator. The broad provisioning PAT mints/rotates
  the narrow one — at bootstrap and on a CI cadence in `secret-rotation.yml`
  (writing the new narrow PAT into each region's OpenBao). This still removes
  the broad PAT from in-cluster *reach* (nothing in-cluster reads it), just not
  from the narrow PAT's *rotation* path.

**Verify before implementing:** an InfoSec/Linode-docs check of whether a
scoped PAT may create tokens. The design works either way; the answer only
decides which component rotates the narrow PAT.

---

## 6. What this retires

Once landed (Phase A always; Phase B when open question #1 resolves):

- **Phase A (decoupled, no apl-core dependency):** the vestigial Path B —
  `secret/certmanager/dns01`, the rotator's `dns-token` table entry, its
  `bao-seed-all` seed, the `dns01-solver-token` ExternalSecret, and the
  `llz ci wait` gate on that Secret (re-point the gate at a real DNS-01
  readiness signal, or drop it). Pure cleanup; nothing that solves DNS-01
  reads Path B.
- **Phase B (gated on §4):**
  - `TF_VAR_linode_dns_token` + the `dns.provider.linode.apiToken` static
    injection in cluster-bootstrap (replaced by the ESO-synced narrow PAT);
  - `secret/linode/api-token`'s in-cluster ESO sync + all in-cluster reads of
    the broad PAT;
  - `llz ci propagate-pat` and the broad-PAT push into clusters;
  - the in-cluster half of the `secret-rotation.yml` PAT rotation — leaving
    only the `TF_STATE_*` OBJ key (tier-3, circular, stays) and, per §5, an
    optional broad-PAT-mints-narrow-PAT step.

Net: the last dual-domain credential leaves the cluster's trust boundary; the
DNS token that actually authorizes challenges finally rotates; and one whole
token + its bootstrap/seed/rotate/ExternalSecret machinery disappears.

---

## 7. Phasing

1. **Phase A — drop the vestigial DNS sub-token.** Independent, safe, no
   apl-core coupling, no new credential. Delivers a real simplification on its
   own and de-risks the confusing two-path state before Phase B touches the
   live DNS path. **Do this first regardless of the rest.**
2. **Spike open question #1** (apl-core existingSecret vs Kyverno) and
   **#2** (scoped-PAT self-mint).
3. **Phase B — the narrow-PAT split + DNS consolidation**, shaped by the spike
   outcomes. Gated on a green Release-E2E that proves ACME issuance still works
   against the rotating narrow PAT (a real cert issued end-to-end), since this
   touches the live DNS-01 path.

---

## 8. Non-goals

- The broad provisioning PAT's **CI** rotation and the `TF_STATE_*` OBJ key
  (tier-3, circular) stay as-is.
- The GitHub-App route (cred-hardening #3) is not required by this design and
  is out of scope; this closes tier-2 without it.
- No change to `events`/`account` scoping on the broad PAT beyond removing its
  in-cluster exposure.
