# The overlay declares it, the cluster does not have it

**Symptom.** A value in `apl-values/_shared/apl-overlay/appvalues.yaml` is
demonstrably correct, the rendered Helm values carry it, the Argo Application
reports `Synced` — and the object does not have it. Usually noticed as a workload
misbehaving in a way the declared value was supposed to prevent.

**The short version.** Argo CD computes its diff by **dry-run-applying** the
desired state. If the API server refuses that apply, no diff is produced; with no
diff the Application keeps its previous verdict (`Synced`) and `selfHeal` never
fires. The failure to apply is what makes the status green. And because the diff
is computed per object, one unappliable field discards every other change to the
same object — including perfectly mutable ones.

A cluster built *after* the change never meets this: the object is created in its
final shape. It is a brownfield-only failure, which is why no e2e lane catches it
(`docs/e2e-gates.md`, archetype ③).

## Diagnose

Everything here is read-only. **Do not run `llz ci converge` to investigate** — it
self-heals with writes to the cluster (strips oversized CRD annotations, restarts
`argocd-redis`, kicks the Harbor provisioner), so it is not a question to put to
production.

```bash
# 1. Could Argo compare the apps at all? A comparison that ERRORS leaves the
#    previous sync status standing, so `Synced` there means nothing. NOTE: a
#    clean result here does NOT clear the cluster — the Loki case below has no
#    condition at all. This step rules a cause IN, never out.
llz ci assert-argo-comparisons

# 2. Did the declared values reach the objects — and if not, would the cluster
#    even accept them? This is the one that answers the question.
llz ci assert-overlay-applied

# 3. Which of those need an object recreated, and where do they stand here?
llz ci brownfield-migrations
```

Step 2 reports one line per mapped field:

| verdict | what it means | what to do |
|---|---|---|
| `DELIVERED` | the object carries the declared value | nothing |
| `APPLIABLE, NOT APPLIED` | the cluster would take it; nothing delivered it | check the owning Application synced — or look for an `UNAPPLIABLE` line on the **same object**, which is discarding this one with it |
| `UNAPPLIABLE` | the API server fixes this field at create time | run the named brownfield migration |
| `REFUSED` | the cluster said no for a reason this gate does not classify | read the apiserver's message in the line before assuming either |
| `UNREADABLE` | the row no longer resolves on the live object | the chart moved what it points at; the field map needs correcting before the gate covers it again |

Paths printed as `?` are **UNCHECKED** — declared, with no row in
`clusterspec.OverlayFields()` mapping them to a live field. They carry the reason
nobody mapped them. Coverage is printed on every run precisely so it is not read
as total.

## Fix

**Converge waits for the result.** Having deleted an object it will not report
convergence until that object is back carrying the value — a health scan cannot
see an undelivered field (that is this whole failure's premise), so "the cluster
looks healthy" is not evidence the recreate landed. If Argo never puts it back,
the run fails naming the object rather than exiting 0 with a StatefulSet missing.

**Usually there is nothing to do.** `llz ci converge` lands pending brownfield
migrations itself, once per run, on the platform scope — the same self-heal
repertoire that realigns `argocd-redis` and strips an oversized CRD annotation. A
fault only a write can clear is one the convergence loop should clear rather than
poll past.

It will not act unless the object can actually come back, and it will not act
twice. Before the delete it writes a record — one key per migration in the
`llz-brownfield-attempts` ConfigMap in `llz-observability` — and a migration with
a record already there is deferred, whatever the cluster looks like now.

The record says an ATTEMPT happened; it never says a migration is done. That is
still read off the live object every time, so the record cannot make an
undelivered value look delivered. Its worst failure is refusing a retry, which is
what `--force` is for (and `--force` clears it, so the next run reads this
attempt's outcome rather than an older one). If the record cannot be written, the
migration does not run: an orphan-delete with no memory of having happened is
worse than a repair that did not.

If you disagree — you have just fixed the render, say — add `--force`:

```bash
llz ci brownfield-migrate --id <id> --yes --force
```

`--force` overrides that judgement and nothing else. It cannot override a read
that failed, or the checks on whether Argo would put the object back: an operator
can know something this code does not about whether a second attempt is
worthwhile, and cannot know what an unanswered apiserver would have said.

Five more checks stand in front of the delete: no poll-level evidence that Argo is wedged (a repo-server
cache auth split, an apply stuck on the annotation limit); the Application that
OWNS the object is `Synced` with no spec error; that Application **self-heals**
(`syncPolicy.automated.selfHeal` — a cluster-side deletion is drift, and nothing
else corrects drift, so without it the delete would be permanent); and its
rendered values **already carry the value being landed** (otherwise the recreate
reproduces the same shape, and the repair would repeat on every run forever). Once an object is
deleted it reads as "nothing to migrate here", so nothing retries it — a delete
into a cluster that cannot recreate is a one-way door. After applying one it
re-polls rather than trusting the verdict it already had, since that reading
predates the delete; and a repair the apiserver refuses fails the run rather than
being annotated past. So the first thing to try is the thing that runs anyway:

```bash
llz ci converge                              # lands what it can, then polls for the result
llz ci converge --brownfield-migrate=false   # observe only, if you want a window
llz --dry-run ci converge                    # ditto: the global flag reaches this write
```

To land one by hand — outside a converge run, or one converge deliberately does
not automate:

```bash
llz ci brownfield-migrations                        # where it stands
llz --dry-run ci brownfield-migrate --id <id> --yes # the plan, writes nothing, exits 0
llz ci brownfield-migrate --id <id> --yes           # does it
```

`--dry-run` is the plan-only form; a bare `--id` with no `--yes` also prints the
plan but exits **non-zero**, because it was asked to do something and declined —
don't use it as a scripted plan step.

The plan names the object, the strategy, what is left to do afterwards, and — if
a precondition would refuse — a `BLOCKED:` line saying which one.

Only migrations declared safe to automate run unattended. One whose repair would
take a workload down is reported by converge and waits for you — the report says
which kind it is.

The `orphan-recreate` strategy deletes the object with `--cascade=orphan` — **the
pods keep running** — waits for Argo to recreate it in the declared shape, and
verifies the field arrived. The recreated object adopts the running pods by
selector.

**The pods then roll themselves, and that is worth understanding before you run
this.** The recreated StatefulSet's template differs from the adopted pods'
revision, so its controller replaces them — the migration does not roll them, but
it does cause them to be rolled. Each replacement loses that ingester's
un-flushed chunks.

For the WAL case that cost is already being paid: the ingesters are
OOM-crashlooping, so those chunks are lost on every restart, and the pods come
back on fresh PVCs with no WAL to replay — which is what ends the loop. For a
migration whose workload is HEALTHY, that reasoning does not transfer; such a
migration should not be marked `Auto`.

Watch the roll rather than driving it:

```bash
kubectl -n monitoring rollout status statefulset/loki-ingester
```

### If the object never comes back

The migration fails loudly and says so. Argo owns the recreate, so check the
owning Application actually synced; the pods are still running and still on the
old spec, so nothing is down while you look.

## Watching for it, rather than finding it

Every cluster publishes its own answer, sampled every five minutes by the
reconciler's `overlay-delivery` lane:

```promql
llz_overlay_field_delivered{path="loki.ingester.resources.limits.memory"} == 0
llz_brownfield_migration_pending == 1
```

No series at all means the lane could not answer — an unreadable apiserver, an
object this cluster does not run, or a row that no longer resolves. That is
deliberate: a `0` in any of those cases would report a delivery failure that is
not one.

## Adding a value to the overlay

`apl-values/_shared/apl-overlay/README.md` carries the contract. In short: every
declared path is mapped in `clusterspec.OverlayFields()` or exempted in
`OverlayUnmapped()` with a reason, and a create-time-only field also names the
brownfield migration that lands it. `TestEveryDeclaredOverlayPathIsMappedOrExempt`
fails a pull request that does neither.

### What checks the `CreateOnly` flag itself

Those guards check the flag is used CONSISTENTLY — a create-only field names a
migration, a mutable one does not. Neither asks whether the classification is
**true**, and it is a hand-set boolean: the next field the apiserver happens to
fix at create time gets `CreateOnly: false` by omission and reproduces the outage
below. Two things ask an apiserver instead.

`llz ci assert-overlay-appliability` is the PR-time half, run in the kind lane of
`lint.yml`. It builds each mapped object in its **pre-overlay** shape, server
-dry-run-patches every declared change, and fails if the apiserver disagrees with
the map in either direction — a field declared mutable that is refused, or a field
declared `CreateOnly` that is accepted. The second is not pedantry: an
over-declared `CreateOnly` is a migration that deletes and recreates a live object
to land a value an ordinary patch would have landed.

    # The fixtures carry NO Namespace objects — the lane they run in owns those, so
    # the namespaces they land in must already exist. `--print-namespaces` lists them
    # rather than leaving this recipe to go stale against a future row.
    for ns in $(llz ci assert-overlay-appliability --print-namespaces); do
      kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
    done
    llz ci assert-overlay-appliability --emit-fixtures --out /tmp/fixtures.json
    kubectl apply -f /tmp/fixtures.json      # a THROWAWAY cluster — this writes objects
    llz ci assert-overlay-appliability

The fixture is seeded from each scalar row's `Prior`, the chart default a
pre-overlay object carries, so the probe tests `default → declared` — the
transition a brownfield cluster actually performs — rather than `absent → set`,
which anything gated on a transition would accept.
`TestEveryScalarRowsPriorIsWhatTheRecordedBrownfieldObjectCarries` holds `Prior`
to `clusterspec/testdata/live/loki-ingester.brownfield.json`; if apl-core moves a
chart default, re-record that file and correct `Prior` with it.

The runtime half is in the migration itself: `createOnlyStillHolds` re-asks the
question of the **live** object immediately before the orphan delete, and refuses
the delete if the apiserver accepts the change — or if it cannot tell. A PR-time
gate protects the next edit to the field map; an instance runs whatever table
shipped in its binary, so the destructive step verifies its own premise.

A skipped row is reported as `skip`, not `ok`, and the pass line says how many
rows went unprobed. If you see that, the row's `Prior` equals its declared value
and the apiserver was never asked about it.

## Worked example — Loki's WAL (`049-loki-wal-pvc`)

The overlay asked for a 3Gi ingester with a 5Gi block-storage-retain WAL claim.
The live `monitoring/loki-ingester` was created before that: 1Gi, `emptyDir`, no
`volumeClaimTemplates`. Adding claim templates to a live StatefulSet is refused:

```
The StatefulSet "loki-ingester" is invalid: spec: Forbidden: updates to
statefulset spec for fields other than 'replicas', 'ordinals', 'template',
'updateStrategy', 'revisionHistoryLimit',
'persistentVolumeClaimRetentionPolicy' and 'minReadySeconds' are forbidden
```

So Argo produced no diff, reported `Synced`, and the **memory limit** — mutable,
under `spec.template`, and the thing that would have stopped the OOM — was
discarded along with it. Two ingesters OOM-crashlooped for weeks behind a green
Application.

**How quiet it actually is, measured on the live cluster.** The Application
carries `sync.status: Synced`, `health: Progressing`, **no conditions at all**,
and an `operationState.finishedAt` twenty days older than the desired state it
claims to have synced:

```
sync=Synced health=Progressing lastApply=2026-08-11T00:19:51Z reconciled=<current>
conditions=
```

That matters for how you diagnose it: there is **no `ComparisonError`**, so
`assert-argo-comparisons` finds nothing here. Only the read-back plus dry run
sees it. The two probes, run against that cluster:

```
$ kubectl -n monitoring patch sts loki-ingester --dry-run=server     -p '{"spec":{"template":{"spec":{"containers":[{"name":"ingester",
        "resources":{"limits":{"memory":"3Gi"}}}]}}}}'
statefulset.apps/loki-ingester patched          # APPLIABLE, NOT APPLIED

$ kubectl -n monitoring patch sts loki-ingester --dry-run=server     -p '{"spec":{"volumeClaimTemplates":[...]}}'
The StatefulSet "loki-ingester" is invalid: spec: Forbidden: ...   # UNAPPLIABLE
```

The live object was at `limits {cpu: 500m, memory: 1Gi}`, `requests {cpu: 250m,
memory: 512Mi}` — apl-core's own defaults. Three of the four declared resource
values had not landed, because they share an object with the claim template.

**The fourth is worth knowing about:** `requests.memory` reads DELIVERED, because
apl-core's default happens to equal the 512Mi the overlay asks for. A green row
is not evidence the overlay reached the object — it can agree with the chart
default by accident. That shape is checked in as a fixture
(`clusterspec/testdata/live/loki-ingester.brownfield.json`) so nobody has to
rediscover it.

## Upstream

Worth reporting to Argo CD, and stated here so the report is not re-derived:

> **A server-side diff that fails to compute should not render as `Synced`.**
> With `ServerSideDiff=true`, the comparison target is produced by a server-side
> apply dry run. When that dry run is REJECTED — e.g. adding
> `volumeClaimTemplates` to an existing StatefulSet — no diff is produced and the
> Application retains its previous `sync.status`. An Application whose comparison
> errored has no current opinion about whether it is in sync, and reporting the
> previous verdict as the current one makes an unappliable desired state
> indistinguishable from a converged one: `selfHeal` never fires, and every status
> field a human or a gate reads says the app is fine.

LLZ turned `ServerSideDiff=true` on deliberately (`clusterspec.CompareOptions`,
#394) to stop CRD defaults and mutating webhooks reading as permanent
unresolvable drift, and the measurements there still hold. This is its one bad
edge; the answer is to detect the edge, not to give the option back.

The apl-core side is narrower: `ingester.persistence.enabled` is offered as an
ordinary chart toggle with no brownfield path. A chart value that can only take
effect on a cluster that does not yet exist should say so.
