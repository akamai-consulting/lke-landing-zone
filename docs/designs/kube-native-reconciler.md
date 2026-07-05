# Design: in-cluster reconciler + convergence metrics surface (watch-based)

**Status:** Phases 0–2 landed incrementally.

- **Phase 0 (merged, #150).** The observe-only foundation:
  [`internal/metrics`](../../tools/internal/metrics/metrics.go) (a dependency-free
  Prometheus text-exposition registry — gauges + counters) and the
  [`llz reconcile`](../../tools/cmd/llz/reconcile.go) command (serves
  `:8080/metrics` + `/healthz`, SIGTERM-graceful), plus the deployable
  default-disabled [`apl-values/components/llzReconciler/`](../../instance-template/apl-values/components/llzReconciler/)
  component (Deployment + read-only RBAC + a default-deny-compatible NetworkPolicy
  that closes the scrape path + Service + ServiceMonitor + PrometheusRule).
- **Phase 1 (merged, #151).** The `internal/kube` watch primitive
  ([`Client.Watch`](../../tools/internal/kube/kube.go) — the Kubernetes watch API
  over raw HTTP, no client-go; borrows only the transport so a long-lived stream
  isn't guillotined by the client's 30s timeout, ctx governs its lifetime).
- **Phase 2 (merged, #152).** The reconciler **manager**
  ([`reconcile_manager.go`](../../tools/cmd/llz/reconcile_manager.go)) — runs N
  named reconcilers with a uniform per-reconciler metric set
  (`llz_reconcile_{runs_total,errors_total,up,last_success_timestamp_seconds,last_duration_seconds}{reconciler}`).
  The observe sampler is now one such reconciler; the two **timed reconcilers** —
  Linode credential rotation and Harbor provisioning — are folded off their
  CronJobs as bounded-resync loops calling the same `ci`-verb logic, **off by
  default**.
- **Watch reconcilers (#153).** The manager gains an **event-triggered**
  loop ([`runWatchReconcilerLoop`](../../tools/cmd/llz/reconcile_manager.go)): a
  reconciler with a `watch` closure runs level-based on each watch event (via
  `Client.Watch`), plus a resync floor, re-establishing the stream on close. The
  first watch reconciler — **argo-resync-nudger**
  ([`reconcile_argo_nudge.go`](../../tools/cmd/llz/reconcile_argo_nudge.go)) —
  watches Argo CD Applications and re-triggers the terminally-failed ones (pure
  Go, `MergePatch`; off by default behind `--reconcile-argo-nudge`; the CronJob
  stays until it proves out). Reacts in seconds vs. the CronJob's up-to-3-min poll.
- **Leader election (#154).** A minimal `coordination.k8s.io` Lease elector
  ([`reconcile_leader.go`](../../tools/cmd/llz/reconcile_leader.go)) over the
  hand-rolled kube client — acquire/renew/take-over/step-down, no client-go
  leaderelection. The observe sampler (read-only) runs on every replica; the
  **driving** reconcilers are gated to no-op unless this replica holds the lease,
  so a rollout window or a scaled-up Deployment can't double-drive. `--leader-election`
  (default on) only engages when a driving reconciler is enabled; a
  `llz_reconcile_leader` gauge + a no-leader alert surface which replica is driving.
- **Node/PV watch reconcilers (this branch).** Two more watch reconcilers, both
  off by default:
  - **cidr-firewall** — watches Nodes and runs the existing `ci discover-firewall-config`
    (already pure Go — the CronJob's only "kubectl" was an annotation-key string)
    to reconcile the CIDR-firewall ConfigMap.
  - **volume-labels** — the Go **port of `relabel.sh`** (new `ci relabel-volumes`
    + `internal/linode.UpdateVolumeLabel`): watches PersistentVolumes and renames
    each bound Linode-CSI Volume to `<region>-<ns>-<pvc>` (lists account Volumes
    once vs the script's per-volume GET). Retires the 103-line embedded-shell blob
    from the `untestable-budget` once the CronJob switches to the new verb.

Still to land: the full convergence gauge (port the `internal/health` classifiers
to feed `llz_convergence_state`) and the credential-age / seal / ESO gauges that
retire the daily port-forward checks; **sc-default-patcher is a deletion
candidate**, not a conversion (the Kyverno `sc-default-demote` mutate-on-write
policy already does it durably). This doc remains the design gate; it touches the
[convergence contract](../architecture/convergence-contract.md) and gets the same
rigor the [linode-credential-rotator](linode-credential-rotator.md) and
[apl-core-v6-migration](apl-core-v6-migration.md) designs got.

**Item:** kube-native next-wave (A) — collapse the fixed-interval polling
CronJob menagerie into one watch-based, leader-elected reconciler, and give the
cluster a Prometheus **convergence metrics surface** so in-cluster observability
(not a daily hosted-runner port-forward) becomes the primary day-2 signal.

**Related designs / issues:** the OpenBao static-seal replacement (issue #30) and
the rotation-off-CI push (issue #27) are *separate* designs and explicitly
**out of scope** here (see Non-goals). This design folds in the continuous
security-audit surface (#99) and the tag-driven volume reaper (#70, #26) only as
future reconcilers that plug into the same manager, not as Phase-1 work.

---

## Problem

The cluster runs **six fixed-interval polling loops**, each a standalone CronJob
with its own ServiceAccount, Role/RoleBinding, and NetworkPolicy:

| CronJob | Interval | What it polls / does | Runs `llz`? |
|---|---|---|---|
| `sc-default-patcher` (foundation) | **2 min** | re-demotes the StorageClass Flux keeps re-promoting | no (Job/kubectl) |
| `argo-resync-nudger` | **3 min** | re-triggers Argo Applications whose last sync `phase=Failed` | no (`/bin/sh` + kubectl) |
| `harbor-robot-provisioner` | **5 min** | ensures Harbor project + robots + publishes creds | `ci harbor-provisioner` |
| `cidrFirewall` discover | **10 min** | Node `providerID` → Linode instance → Cloud Firewall → ConfigMap | `ci discover-firewall-config` |
| `volumeTagReconciler` | **hourly** | reconciles StorageClass `volumeTags` onto Linode Volumes | `ci reconcile-volume-tags` |
| `linodeCredRotator` | **daily** | age-based rotation of Loki/Harbor object-storage keys into OpenBao | `ci rotate-linode-creds` |

…**plus** the daily [`scheduled-checks`](../../instance-template/.github/workflows/scheduled-checks.yml)
health jobs that **port-forward from a hosted GitHub runner** to poll OpenBao
seal state, ESO `ClusterSecretStore` readiness, cert-manager `Certificate`s, and
Prometheus rule-group load errors — asking the in-cluster Prometheus questions
from *outside* the cluster, once a day.

Three costs fall out of this shape:

1. **MTTR = poll interval.** A terminally-failed Argo sync during the converge
   window sits dead for up to 3 minutes; a StorageClass re-promotion for up to 2;
   an object-storage key past its rotation SLA for up to a day. These are
   *level-triggered* problems (an object changed, or a deadline passed) solved by
   *edge-polling on a timer*.
2. **The cluster can't self-report.** [alerting.md](../alerting.md) is explicit
   that today the only signal reaching a human is GitHub Actions annotations —
   Alertmanager runs but defaults to a null receiver. And
   [lessons-learned](../lessons-learned.md) flags that the hosted runners doing
   the daily health port-forwards may be **CIDR-blocked** from the LKE API /
   OpenBao. Every check that heals or observes the cluster *from outside* is one
   firewall rule away from going blind.
3. **Sprawl.** Six SAs, six RBAC bundles, six NetworkPolicies, two embedded
   shell scripts spending the [`untestable-budget`](../../.untestable-budget.yaml).

The substrate to fix this **already exists**: `build-images.yml` already publishes
`llz` as a slim distroless image, and these very CronJobs already run it. The
reconcile *logic* is already Go and already unit-tested (the untestable-budget
ratchet paid for that). What's missing is a long-lived process that changes the
*trigger* from a timer to a watch, and a metrics surface that lets the cluster
speak for itself.

---

## Goals / non-goals

### Goals

- Replace fixed-interval polling with **watch-based (informer) reconciliation**
  wherever the trigger is a Kubernetes object change; keep a **bounded periodic
  resync** only where the authoritative state lives outside the cluster (Linode
  API, Harbor) and emits no events.
- Expose a **Prometheus metrics surface**: the existing `llz ci health` 0/1/2
  convergence classification (plus the transient exit-3 apiserver-unreachable
  state) as gauges, plus per-reconciler last-success timestamps and
  credential-age gauges — so `PrometheusRule`s + Alertmanager become the primary
  day-2 signal and the daily hosted-runner health port-forwards retire.
- **Consolidate** six workloads into one leader-elected Deployment with one SA /
  RBAC / NetworkPolicy set.
- Stay **strictly inside the convergence contract**: the metrics surface
  *observes*; reconcilers that *drive* (nudger, sc-patcher) keep exactly the
  behavior they have today — event-triggered instead of timer-triggered, no new
  driver.

### Non-goals

- **No new CRD or custom API.** The contract forbids standing up our own
  operators (we consume upstream CRDs; see the in-cluster stack inventory). This
  reconciles **existing** objects (`Application`, `Node`, `PVC`/`PV`, `Secret`,
  `StorageClass`) and Linode-side state. No `apiextensions`, no Kubebuilder API
  types.
- **No client-go, no controller-runtime, no `prometheus/client_golang`.** The
  `tools/` module is deliberately lean — [`internal/kube`](../../tools/internal/kube/kube.go)
  is a hand-rolled REST client ("no kubectl, no client-go") built for the slim
  distroless image, and `internal/*` holds pure, unit-tested logic while `cmd/llz`
  wires the I/O. This design **stays inside that stance**: the watch is the
  Kubernetes **watch API over raw HTTP** (a `?watch=true` chunked-JSON stream),
  added to `internal/kube` in the same style; the metrics surface is a
  hand-rolled Prometheus **text-exposition** writer (`internal/metrics`), not the
  client library. Pulling controller-runtime's transitive tree onto the
  distroless image would violate the constraint that makes that image viable —
  so it is explicitly rejected, not merely unused.
- **Not** the OpenBao static-seal replacement — that on-disk-key anti-pattern
  (issue #30) is its own design; this reconciler *observes* seal state as a
  metric but does not change how unseal works.
- **Not** a change to rotation trust boundaries. `lke-admin` rotation stays
  external (LKE-E `delete-kubeconfig` API — a closed decision in
  [lessons-learned](../lessons-learned.md)); broadening in-cluster rotation is
  issue #27's design.
- **Not** a replacement for `llz ci health` as the Terraform readiness gate or
  the `ci converge` bootstrap wrapper. That CLI stays the exit-code source of
  truth (contract §2); the controller *reuses* `internal/health` predicates and
  exports the same classification as a metric. One predicate library, two
  consumers (CLI exit code for gates, gauge for day-2 observability) — never two
  competing authorities.

---

## Watch vs. resync — the honest classification

Not every loop is a pure watch. The design is **"watch where events exist;
bounded resync where they don't"** — and even the resync cases come out ahead
(workqueue dedup + exponential backoff + one leader, instead of fixed-interval
fire-and-forget).

| Current CronJob | Real trigger | Watch source | Resync floor | Verdict |
|---|---|---|---|---|
| `argo-resync-nudger` | Application enters `operationState.phase=Failed` | **watch `Application`** | none needed | poll→watch; reacts in seconds, not up to 3 min of dead converge time. Scope unchanged (terminal failures only). **Keep, convert.** |
| `sc-default-patcher` | `is-default-class` flipped on the SC | **watch `StorageClass`** | none needed | poll→watch. But the Kyverno `sc-default-demote` *mutate-on-write* policy is the durable form — the controller is a backstop; **candidate for deletion** once the policy is trusted, not just conversion. |
| `cidrFirewall` discover | Node added/removed / `providerID` change | **watch `Node`** | Linode Firewall drift → small resync (~10 min) | poll→watch on Node events; resync covers out-of-band firewall edits. |
| `volumeTagReconciler` | PVC bound → PV gets a Linode Volume | **watch `PV`/`PVC`** | Linode Volume drift → hourly resync | reconcile on bind event; resync covers out-of-band tag drift. |
| `harbor-robot-provisioner` | Harbor project/robot drift (external API) | secret-consumer watch, mostly resync | Harbor API → ~5 min resync | mostly resync — external state, no k8s events. Still consolidated + backoff. |
| `linodeCredRotator` | **age deadline** (not an object) | none — timed | daily resync | stays time-driven; folds into the manager's periodic resync. Rotation is inherently time-based. |

The loop handles both: a reconciler consumes its `internal/kube` watch stream
**and** carries a resync-floor timer. The two genuinely time-driven cases (cred
rotation, Harbor) are strictly no worse than today and gain leader election +
backoff.

---

## Proposed design

A single leader-elected **Deployment `llz-reconciler`** (1 active replica) running
a new `llz reconcile` subcommand: a hand-rolled loop over `internal/kube` watch
streams + bounded resync timers, in the module's existing pure-Go style.

- **Reuses the code that already exists.** Each `ci <verb>` reconcile function
  (`ci reconcile-volume-tags`, `ci discover-firewall-config`,
  `ci rotate-linode-creds`, `ci harbor-provisioner`) becomes a reconcile handler
  invoked on a watch event (or resync tick) instead of by a CronJob. The logic —
  already in
  [`tools/internal/`](../../tools/internal/) (`kube`, `linode`, `health`,
  `openbao`) and unit-tested — does not move; only the *invocation* changes from
  "CronJob calls the CLI once" to "manager calls the reconciler on an event."
  The nudger's and sc-patcher's embedded `/bin/sh` scripts convert to Go,
  reclaiming `untestable-budget`.
- **Metrics surface.** A hand-rolled `internal/metrics` text-exposition registry
  (no `prometheus/client_golang`) served on `:8080/metrics`. Gauges:
  - `llz_convergence_state{app="…"}` — 0/1/2/3, sourced from the same
    `internal/health` predicates the CLI uses;
  - `llz_reconcile_last_success_timestamp{reconciler="…"}` and
    `llz_reconcile_errors_total{reconciler="…"}`;
  - `llz_credential_age_days{cred="loki-object-store|harbor-registry-s3|…"}` —
    replaces the daily `health-loki-objkey-rotation` / `health-lke-admin-rotation`
    SLA port-forwards;
  - `llz_openbao_sealed{pod="…"}`, `llz_eso_store_ready{store="…"}` — replace the
    daily `health-openbao` port-forward.
  A `ServiceMonitor` scrapes it; `PrometheusRule`s alert on hard-fail
  (`state==1`), staleness (`time() - last_success > threshold`), and SLA breach
  (`credential_age_days > limit`). This is the inversion the alerting doc calls
  for: **cluster-native primary, CI belt-and-suspenders secondary.**
- **Single-writer guarantee.** The Linode-side reconcilers must not double-mint
  keys or double-tag Volumes — the CronJobs get this today via
  `concurrencyPolicy: Forbid`. The manager gets it *better* via leader-election
  `Lease` + a deduplicating workqueue. The rotator's existing idempotency
  (verify-before-write, keep-newest-2) remains the belt to that suspenders.

### Components (skeleton — `apl-values/components/llzReconciler/`)

Mirrors the `linodeCredRotator` component layout: a Deployment, a ServiceAccount,
one Role/ClusterRole + binding (union of the six CronJobs' verbs, reviewed down),
one NetworkPolicy (egress: apiserver + Linode API + OpenBao; ingress: Prometheus
scrape only), a `ServiceMonitor`, and the `PrometheusRule` set. Enabled per-env
via `spec.components` like the other components.

### Convergence-contract compliance (the part reviewers will check first)

- **The metrics surface observes, it does not drive** — it reads Argo Application
  health and exports it. That is contract-clean; anti-pattern #4 is about *adding
  a side-controller to drive what a reconciler should observe*, which this is the
  opposite of.
- **The nudger and sc-patcher already drive today.** Converting poll→watch does
  **not** add a new driver — it makes an existing, documented one event-triggered.
  The nudger's scope is unchanged: only Applications whose sync `phase=Failed`,
  which Argo genuinely will not self-heal (`selfHeal` only corrects drift *after*
  a successful sync — see the nudger's own header). No new
  `annotate force-sync=$(date)` appears anywhere (anti-pattern #6); the nudger's
  existing annotate is byte-for-byte what it does now.
- **`llz ci health` stays the exit-code SoT** for the TF gate and `ci converge`
  (contract §2). The controller shares its predicate library and never becomes a
  competing authority.

---

## Bootstrap / cold-start

The reconciler is **day-2 + defense-in-depth**, the same role the nudger and
CronJobs play today — its absence must never wedge first-boot. During bootstrap,
the TF converge gate (`ci converge`) plus Argo's generous retry budgets and the
lenient ESO/NetworkPolicy health overrides still own convergence.

One real sequencing question: the `argo-resync-nudger` is currently
`sync-wave: -18` and **load-bearing during the converge window** (a terminal
first-boot ordering race is the exact case it exists for). So the reconciler
Deployment — or at least its nudger reconciler — must be up at least as early as
the current CronJob. Land it at the same low wave, after the Application CRD is
registered (`apl_pipeline_ready`), and confirm in e2e that the watch is
established before the first components sync. See Open questions.

---

## Failure modes

- **Reconciler down → reconciliation stops.** Mitigation is the *inverted*
  layering: the CI `scheduled-checks` become the belt-and-suspenders (cluster
  primary, CI backup — the reverse of today), a
  `llz_reconcile_last_success_timestamp` staleness alert fires, and every
  reconciler is a bounded resync so a restart re-converges. The `ci <verb>`
  functions stay invokable from CI as manual break-glass — they don't disappear,
  they lose their CronJob wrapper.
- **Split brain / double leader** → `Lease` election; Linode reconcilers stay
  idempotent (verify-before-write, keep-newest-2 — already true in the rotator).
- **Watch storm / hot loop** → a rate-limited work queue + exponential backoff
  in the reconcile loop — strictly better than a fixed 2-minute fire.
- **Linode API rate limits** → per-reconciler resync floor; the rotator already
  carries this concern.
- **Informer cache staleness on the health predicates** → the `internal/health`
  predicates today assume a one-shot kubeconfig client; reading them off a
  long-lived cached client is a bounded refactor (Open questions).

---

## Observability

The metrics surface *is* the observability deliverable — it's not a side effect.
Success criterion for Phase 0 is: every signal the daily `scheduled-checks`
port-forward jobs produce today has an equivalent in-cluster gauge + PrometheusRule,
so those jobs can be demoted to belt-and-suspenders (Phase 3). `gh-pat-expiry`,
`go-vuln-audit`, and `template-drift` stay in CI — they need no cluster access
and aren't cluster state.

---

## What this retires (once enabled + e2e-validated)

- Six CronJobs and their per-job SA / Role / NetworkPolicy → one Deployment +
  one SA / RBAC / NetworkPolicy.
- The `argo-resync-nudger` and `sc-default-patcher` embedded shell scripts →
  Go (untestable-budget win).
- The daily `scheduled-checks` port-forward health jobs (`openbao-health`,
  `certmanager-health`, `prometheusrule-health`, `lke-admin-rotation-health`,
  `loki-objkey-rotation-health`) → in-cluster metrics + PrometheusRules (demoted
  to belt-and-suspenders, not deleted, per the alerting-doc layering).
- Possibly `sc-default-patcher` entirely, if the Kyverno mutate-on-write policy
  is confirmed durable on its own.

Discipline (from the cred-rotator doc): **keep every CronJob manifest until its
watch replacement passes one green e2e cycle**, then delete in the same PR that
proves it.

---

## Rollout (phased — lowest-risk / highest-CIDR-payoff first)

- **Phase 0 — metrics only, drive nothing.** `llz reconcile` manager skeleton +
  the metrics surface + ServiceMonitor + PrometheusRules, running *alongside* the
  existing CronJobs. Export convergence + credential-age + seal/ESO gauges. This
  validates the alerting path and lets us demote the CIDR-fragile daily health
  port-forwards first — highest payoff, lowest risk (observe-only).
- **Phase 1 — pure-watch reconcilers.** Migrate `argo-resync-nudger`,
  `sc-default-patcher`, `cidrFirewall`, `volumeTagReconciler` one at a time, each
  behind its own component enable flag; delete each CronJob only after a green
  e2e cycle.
- **Phase 2 — timed reconcilers.** Fold in `linodeCredRotator` and
  `harbor-robot-provisioner` as bounded-resync controllers; retire those CronJobs.
- **Phase 3 — flip CI to belt-and-suspenders.** Once in-cluster alerting is
  trusted, reduce the `scheduled-checks` cadence and mark the cluster-observable
  ones non-blocking.
- **Later (out of scope here):** the continuous security-audit surface (#99, as
  `PolicyReport` gauges) and the tag-driven volume reaper (#70/#26) plug in as
  additional reconcilers under the same manager.

---

## Open questions

1. **Namespace.** New `llz-system` vs. reuse `llz-observability`? The manager
   needs egress to apiserver + Linode API + OpenBao, and ingress from Prometheus.
2. **Image.** Reuse the existing distroless `llz` image (the reconciler adds no
   heavy deps — it's the same `internal/kube` client plus a stdlib HTTP server),
   so the same image the CronJobs run should carry `llz reconcile` unchanged.
   Confirm the binary-size delta is negligible.
3. **Health predicates on a cached client.** Do `internal/health`'s predicates
   read cleanly off a long-lived informer cache, or do they assume a fresh
   list-per-call? Size the refactor before Phase 0.
4. **Nudger bootstrap sequencing.** Confirm in e2e that the watch is established
   early enough to cover the nudger's load-bearing converge-window role (it's
   `sync-wave -18` today for a reason).
5. **Keep the nudger at all?** It's defensible (Argo won't retry terminal
   failures) — the watch version just reacts faster. Recommendation: **keep,
   convert** — do not delete.
6. **Leader-election Lease** location + the minimal RBAC for it.

---

## See also

- [convergence-contract.md](../architecture/convergence-contract.md) — the
  exit-code model and the anti-patterns this design is written to honour.
- [linode-credential-rotator.md](linode-credential-rotator.md) — the existing
  in-cluster reconciler this generalizes; its phased-rollout discipline is the
  template for the Rollout section.
- [alerting.md](../alerting.md) — the null-receiver default and the CI-vs-cluster
  layering this design inverts.
- [.untestable-budget.yaml](../../.untestable-budget.yaml) — the ratchet the
  nudger/sc-patcher shell conversions pay down.
