# Upstream asks — apl-core / Linode Managed App Platform

Things this repo **cannot fix in this repo**, because the managed App Platform owns
the component. Each entry says what breaks, what LLZ does about it in the meantime,
and what would let the workaround be deleted.

This page exists because the alternative kept happening: an override written into
LLZ that plausibly addresses an apl-core problem, applies to nothing, and is
believed for months. If a fix belongs upstream, it belongs here — as a named ask
with a live gate holding the line — not as a values key nobody can verify.

**How to use it.** Before adding a workaround for apl-core behaviour, add the row
first. Before deleting a workaround, check its exit condition here.

---

## 1. Loki's ingester OOMKills mid-WAL-replay, and cannot recover

**Severity: log ingestion down indefinitely, and it does not self-heal.**

apl-core runs the grafana/loki chart in its distributed topology. The ingester
ships with a memory limit too low to replay an accumulated write-ahead log, and its
WAL lives on an `emptyDir`. An `emptyDir` survives *container* restarts within the
pod, so an ingester OOMKilled mid-replay comes back, replays the identical WAL, and
dies identically. Nothing about the loop is self-limiting.

Measured on a production cluster: `loki-ingester-1` logged **104,337** BackOff
events over **16 days**, with log ingestion down throughout and the platform
convergence gate failing on every promote. The only escape was deleting the pod,
which discards un-flushed chunks.

**What LLZ does today.** Asserts `ingester.resources.limits.memory` and
`ingester.persistence` through the apl-overlay
(`apl-values/_shared/apl-overlay/appvalues.yaml`), and holds the *running* ingester
to that floor with `llz ci assert-loki` — which reads the pod, not the values, so a
key that names nothing in the chart shows up as a red lane instead of silence.

**The ask.** apl-core should ship an ingester whose defaults survive their own WAL:
a limit sized for replay, and a real PVC rather than an `emptyDir`, so the failure
mode is a restart rather than a permanent crashloop. LLZ's override then becomes
redundant rather than load-bearing.

**Exit condition.** apl-core's default ingester passes `llz ci assert-loki`'s
durability check with the overlay entry removed. Delete
`lokiIngesterValues()` in `tools/internal/shared/clusterspec/overlay_appvalues.go`
and keep the assertion.

### The WAL PVC needs a one-time manual migration on existing clusters

**Read this before merging the overlay change onto a live cluster.**

`volumeClaimTemplates` is **immutable** on an existing StatefulSet. Turning
`ingester.persistence.enabled` on therefore cannot be applied in place: the
apiserver rejects the update (`updates to statefulset spec for fields other than
'replicas', 'ordinals', 'template', 'updateStrategy',
'persistentVolumeClaimRetentionPolicy' and 'minReadySeconds' are forbidden`), the
sync fails, and the ingesters keep their `emptyDir` WAL. `llz ci assert-loki` will
report that — correctly and persistently — until someone migrates.

That red lane is the truth, not a bug in the gate: an `emptyDir` WAL *is* the
condition that makes an OOM crashloop unrecoverable. But it does not clear on its
own, so:

```bash
# One ingester StatefulSet, one time, per cluster. Orphan the pods so ingestion
# keeps running while the StatefulSet is recreated with volumeClaimTemplates.
kubectl -n monitoring delete statefulset loki-ingester --cascade=orphan
# Let Argo CD re-create it (or force a sync). The new StatefulSet adopts the
# existing pods by name; each pod picks up its PVC as it is next restarted.
kubectl -n monitoring rollout status statefulset/loki-ingester --timeout=10m
llz ci assert-loki
```

**Un-flushed chunks on a pod that restarts into a fresh PVC are lost** — the same
cost as the emergency `delete pod` that breaks an active crashloop. Do it when
ingestion is healthy, not during an incident.

A cluster whose ingesters are *already* crashlooping is the easier case: the WAL
they cannot replay is worthless, so orphan-delete and let them come back on PVCs.

---

## 2. `accessKeyId` is inlined from settings, not sourced from ESO

Tracked upstream as **linode/apl-core#3459**.

apl-core reads the object-storage `secretAccessKey` from the `obj-secrets` Secret
via ESO — which is why LLZ can keep it out of git entirely. `accessKeyId` gets no
such treatment: apl-core inlines it from `env/settings/obj.yaml`, so the committed
overlay has to carry a placeholder that the in-cluster reconciler substitutes on
every pass.

**What LLZ does today.** `ObjAccessKeyIDPlaceholder` in the committed overlay, filled
by the apl-overlay reconciler from OpenBao `secret/obj/platform`. It never resolves
on `main`, so nothing but a placeholder is committed.

**Exit condition.** apl-core sources `accessKeyId` from `obj-secrets` alongside the
secret half. The placeholder and `FillObjPlaceholders` both go away.

---

## 3. No confirmed channel for an apl-core app's first-class chart keys

LLZ can set an apl-core app's `_rawValues` — the apl-overlay reconciler merges them
onto apl-operator's own `env/apps/<name>.yaml` AplApp CR, which is
lab-confirmed to carry that field and to preserve it across apl-operator's
rewrites. What is **not** confirmed is the same treatment for an AplApp CR's
first-class keys (`retention`, `storageSize`, `replicas`, and the like).

**What breaks.** Four spec knobs — `observability.retention`, `observability.storage`,
`observability.replicas` and `harbor.registryStorage` — are parsed, merged,
validated, and shown by `llz apl app list`, and render nowhere. Their only render
target was the per-env `apl-values/<env>/values.yaml` that LLZ stopped emitting at
the managed pivot. An operator who sets one changes the spec and nothing else.

**What LLZ does today.** Names them in `clusterspec.InertSpecFields` so
`llz doctor` reports them when an instance actually sets one, rather than accepting
them in silence. A test pins each against the render output, so wiring one forces
its removal from that list.

**The ask.** Confirm (or document) apl-core's AplApp schema for these keys and
whether an external writer may set them on the machine branch without apl-operator
reverting them. That is a question about apl-core internals which only a live
cluster or apl-core itself can answer.

**Exit condition.** Each knob renders into the overlay and leaves
`InertSpecFields`.

---

## 4. Alertmanager receivers have no channel LLZ can write

**Severity: every alert this repo ships pages nobody by default, including the ones
that catch a total outage.**

apl-core renders the full Alertmanager route/receiver config from a top-level
`alerts:` values block. That is not a per-app key, so the apl-overlay's per-app
channel cannot reach it, and the per-env values file that used to carry it is gone.

`spec.alerting` — receivers, Slack channel, critical channel — is therefore
validated and rendered nowhere. `llz ci assert-alert-delivery` already states the
consequence plainly: it proves Prometheus reaches Alertmanager and deliberately
does **not** assert Alertmanager reaches a human, "because apl-core owns the
receiver configuration and this repo ships none".

**What LLZ does today.** `InertSpecFields` (as above) plus a `doctor` finding, so an
operator who configures Slack is told it reaches nothing instead of assuming it
works. The webhook secret half already works: `secret/alerts/webhooks` is seeded and
rotated, and the `kyverno-alertmanager-slack-webhook` policy repoints apl-core's
`alertmanager-credentials` ExternalSecret at OpenBao.

**The ask.** Either an `AplAlerts`-style settings CR under `env/settings/` that an
external writer may own (as `AplObjectStorage` already is for object storage), or a
documented supported path for setting `alerts.receivers` on a managed cluster.

**Exit condition.** `spec.alerting` renders into the overlay, leaves
`InertSpecFields`, and `assert-alert-delivery` can drop its stated scope caveat.

---

## Retired asks

- **`apps.loki.adminPassword` was `required` in apl-core ≤ 6.1.0**, which forced LLZ
  to supply a credential it did not want to own before apl-core's own generator
  could fire. Dropped from `required` in **linode/apl-core#3465** (6.2.0); LLZ's
  workaround is gone.
