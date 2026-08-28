# Loki Access — Playbook

**Applies to:** Loki on every cluster, backed by Linode Object Storage. apl-core runs
the chart in its **distributed** topology — a `loki-ingester` StatefulSet plus separate
`loki-querier` / `loki-distributor` / `loki-compactor` / `loki-gateway` workloads, all in
the `monitoring` namespace. Confirm before acting on any of it:

```bash
kubectl -n monitoring get statefulset,deploy -l app.kubernetes.io/name=loki
```

(This page described a `SingleBinary` deployment for as long as apl-core has not run
one. The distinction is not cosmetic — chart values under `singleBinary.*` are ignored
outside single-binary mode, which is how a Loki OOM fix sat in the tree for months
applying to nothing.)

**Related:** [`grafana-access.md`](grafana-access.md).

---

## How access works

Two facts shape every Loki playbook:

1. **No external Ingress.** Loki is reachable only as `http://<release>-loki-gateway.monitoring.svc.cluster.local` — inside the cluster network. Operators reach it via Grafana (preferred) or `kubectl port-forward` (debug).
2. **Multi-tenancy is ON, and the tenant partitions reads.** apl-core ships Loki with
   `auth_enabled: true`, so **every** request must carry an `X-Scope-OrgID` header. A
   read without one answers `no org id`; a read with the *wrong* one returns an empty
   result set, which looks exactly like "the writer is dead".

### Which tenant am I?

There is more than one writer and they do not share a tenant, so there is no single
correct value — pick the one belonging to the logs you are looking for:

| Logs you want | Tenant | Why |
|---|---|---|
| Anything in a landing-zone namespace (`llz-*`, `argocd`, `harbor`, `external-secrets`, …) | **`admins`** | apl-core's collector routes by namespace; everything outside a team namespace lands in the default pipeline |
| Workloads in `team-<name>` namespaces | **`<name>`** | e.g. `team-platform` → `platform`, `team-admin` → `admin` |
| OpenBao audit log | **`platform`** | the OpenBao pod's promtail sidecar pushes with `tenant_id: platform`, independently of the collector |

Reading the OpenBao audit stream as `admins` (or the reconciler's logs as
`platform`) returns nothing and reports a healthy pipeline as dead. `llz ci
assert-openbao-audit` and `llz ci assert-log-ingestion` each default to the right
tenant for what they check — prefer them over a hand-rolled query.

---

## Operator access — via Grafana (canonical)

Grafana is the supported read path: it carries the tenant header for you, ships with the Loki data source pre-configured, and lets you build LogQL queries interactively.

1. Port-forward Grafana and log in — see [`grafana-access.md`](grafana-access.md).
2. *Explore → Data source: Loki*.
3. Write LogQL — e.g.:

    ```logql
    {app="<release>-app"} |= "error"
    {namespace="llz-openbao"} |~ "(?i)sealed"
    sum by (level) (count_over_time({app="<release>-app"}[5m]))
    ```

The Grafana → Loki data source uses the cluster-internal Service URL with an
`X-Scope-OrgID` custom HTTP header. Whichever tenant that data source carries is the
one you are querying — check it under *Connections → Data sources → Loki* if a query
you expect to match returns nothing, and add a second data source (same URL,
different header) rather than editing the shared one when you need another tenant.

---

## Operator access — direct (debug)

When Grafana itself is broken, or you want to script queries:

```bash
# 1. Port-forward Loki's HTTP gateway
kubectl -n monitoring port-forward svc/<release>-loki-gateway 3100:80

# 2. LogQL via the HTTP API. TENANT is mandatory — see "Which tenant am I?" above.
#    `admins` covers every landing-zone namespace; the OpenBao audit log is `platform`.
TENANT=admins
curl -G "http://localhost:3100/loki/api/v1/query_range" \
  -H "X-Scope-OrgID: ${TENANT}" \
  --data-urlencode 'query={namespace="llz-openbao"}' \
  --data-urlencode "start=$(date -u -v-1H '+%Y-%m-%dT%H:%M:%SZ')" \
  --data-urlencode "end=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  --data-urlencode 'limit=100' \
  | jq

# Useful endpoints (all tenant-scoped — send the header on every one):
#   GET /loki/api/v1/labels                  — list label names
#   GET /loki/api/v1/label/<name>/values     — list label values
#   GET /loki/api/v1/query_range             — range query (LogQL)
#   GET /loki/api/v1/query                   — instant query
#   GET /ready, /metrics                     — health (not tenant-scoped)
```

> **`date -u -v-1H` is BSD/macOS.** On Linux use `date -u -d '1 hour ago'`.

The two failure modes, which look nothing alike:

- **No header** → HTTP 401 `no org id`. Add one.
- **Wrong header** → HTTP 200 with an empty `result` array. Nothing is wrong with
  the query, the writer, or the pipeline — you are reading a tenant those logs were
  never written to. Check the table above before debugging anything else.

---

## Write path

You should not normally write to Loki by hand. The two production writers are:

- **apl-core's platform-logs-collector** — routes by namespace via its `routing`
  connector: `team-admin` → `admin`, `team-platform` → `platform`, and everything
  else (including every landing-zone namespace) → **`admins`**. Gated by
  `llz ci assert-log-ingestion`, which defaults to that tenant for exactly this
  reason.
- **Promtail sidecar in the OpenBao pod** — tails `/openbao/audit/audit.log` and pushes to the same gateway with `tenant_id: platform`, independently of the collector. See the audit-logging notes in [`docs/secrets.md`](../secrets.md#audit-logging). That write path is gated end to end by `llz ci assert-openbao-audit`, which queries `{app="openbao",component="audit"}` over the last 30 minutes and fails if nothing arrived — run it (it is read-only) before hand-debugging a "no audit logs" report; its failure output names the five things to check, in order.

Because those two use different tenants, **no single header reads both**. That is
why the two gates carry separate tenant defaults rather than sharing one constant.

If you need to push test logs manually — they land in whichever tenant you name, so
read them back with the same one:

```bash
curl -fsSL -X POST "http://localhost:3100/loki/api/v1/push" \
  -H "X-Scope-OrgID: admins" \
  -H "Content-Type: application/json" \
  -d '{
    "streams": [{
      "stream": {"app": "manual-test", "level": "info"},
      "values": [["'"$(date +%s%N)"'", "hello from a manual push"]]
    }]
  }'
```

---

## Tenancy expansion

Tenants already exist per APL team — declaring a team in `spec.teams` gets its
namespace routed to its own tenant by the collector, with no Loki-side change. Reach
for the steps below only for a tenant that is **not** an APL team:

1. Set its writers to send `X-Scope-OrgID: <new-tenant>`.
2. Add a per-tenant `limits_config` block to Loki's chart values — see Loki's
   [multi-tenancy docs](https://grafana.com/docs/loki/latest/operations/multi-tenancy/)
   for ingestion-rate / retention overrides. **Where**: `apps.loki._rawValues` in
   `apl-values/_shared/apl-overlay/appvalues.yaml`, which is rendered — edit
   `argoHealthCustomizations`'s sibling `lokiIngesterValues` in
   `tools/internal/shared/clusterspec/overlay_appvalues.go` and re-run `llz render`.
   That overlay is the only channel that reaches apl-core's values on the managed
   platform; anything you put in `apl-values/values.yaml` reaches no cluster (that file
   no longer exists, for exactly this reason).
3. Add a second Loki data source in Grafana for that tenant (header value differs).

Don't reuse `admins` as a catch-all for workload logs — once they are mixed into the
platform's default tenant, splitting them out later is painful.

---

## SLA + rotation

The bucket-access key Loki uses to talk to Linode Object Storage is `secret/loki/object-store` in OpenBao, rotated in-cluster by the `linodeCredRotator` lane on a ~80-day cadence — NOT by Terraform (the TF-managed keys and their `time_rotating` clock were removed). Drift is caught two ways, both off the reconciler's `llz_credential_age_days{cred="loki-object-store"}` gauge: the continuous `LLZCredentialRotationOverdue` alert (>90d) and the daily `llz ci assert-rotation-health` gate in the `credential-single-pane` job, which fails on an absent series as well as an overdue one. (A weekly `loki-objkey-rotation-health` step used to sit here too; it needed a root token bootstrap revokes, so it never produced a verdict, and #483 retired it.) See [`docs/runbooks/linode-credential-rotation.md`](../runbooks/linode-credential-rotation.md) for the manual reseed procedure if it ever falls behind.
