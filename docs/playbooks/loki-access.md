# Loki Access — Playbook

**Applies to:** Loki (`<release>-loki` SingleBinary deployment in the `observability` namespace) on every cluster. Backed by Linode Object Storage per cluster.

**Related:** your observability configuration, [`loki-values.yaml`](../../instance-template/apl-values/example/values.yaml), [`grafana-access.md`](grafana-access.md).

---

## How access works

Two facts shape every Loki playbook:

1. **No external Ingress.** Loki is reachable only as `http://<release>-loki-gateway.observability.svc.cluster.local` — inside the cluster network. Operators reach it via Grafana (preferred) or `kubectl port-forward` (debug).
2. **Multi-tenancy is on.** `loki.auth_enabled: true` in [`loki-values.yaml`](../../instance-template/apl-values/example/values.yaml). Every read or write **must** carry the header `X-Scope-OrgID: <project>` — Loki returns 401 / "no org id" otherwise.

The single tenant we use today is **`<project>`** ([`loki-values.yaml`](../../instance-template/apl-values/example/values.yaml), [`grafana-values.yaml`](../../instance-template/apl-values/example/values.yaml), [`otel-collector-values.yaml`](../../instance-template/apl-values/example/values.yaml)).

---

## Operator access — via Grafana (canonical)

Grafana is the supported read path: it carries the tenant header for you, ships with the Loki data source pre-configured, and lets you build LogQL queries interactively.

1. Port-forward Grafana and log in — see [`grafana-access.md`](grafana-access.md).
2. *Explore → Data source: Loki*.
3. Write LogQL — e.g.:

    ```logql
    {app="<release>-app"} |= "error"
    {namespace="openbao"} |~ "(?i)sealed"
    sum by (level) (count_over_time({app="<release>-app"}[5m]))
    ```

The Grafana → Loki connection uses the cluster-internal Service URL with `X-Scope-OrgID: <project>` injected as a custom HTTP header (see `grafana-values.yaml`).

---

## Operator access — direct (debug)

When Grafana itself is broken, or you want to script queries:

```bash
# 1. Port-forward Loki's HTTP gateway
kubectl -n observability port-forward svc/<release>-loki-gateway 3100:80

# 2. LogQL via the HTTP API — note the mandatory X-Scope-OrgID header
curl -G "http://localhost:3100/loki/api/v1/query_range" \
  -H "X-Scope-OrgID: <project>" \
  --data-urlencode 'query={app="<release>-app"} |= "error"' \
  --data-urlencode "start=$(date -u -v-1H '+%Y-%m-%dT%H:%M:%SZ')" \
  --data-urlencode "end=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  --data-urlencode 'limit=100' \
  | jq

# Useful endpoints:
#   GET /loki/api/v1/labels                  — list label names
#   GET /loki/api/v1/label/<name>/values     — list label values
#   GET /loki/api/v1/query_range             — range query (LogQL)
#   GET /loki/api/v1/query                   — instant query
#   GET /ready, /metrics                     — health
```

Forgetting the header is the most common debug-time mistake; the API returns a useless-looking `no org id` 401 with no other context.

---

## Write path

You should not normally write to Loki by hand. The two production writers are:

- **OTel Collector** (see `otel-collector-values.yaml`) — exports OTLP logs to `loki-gateway` with `X-Scope-OrgID: <project>` baked in.
- **Promtail sidecar in the OpenBao pod** — tails `/openbao/audit/audit.log` and pushes to the same gateway. See the audit-logging notes in [`docs/secrets.md`](../secrets.md#audit-logging).

If you need to push test logs manually:

```bash
curl -fsSL -X POST "http://localhost:3100/loki/api/v1/push" \
  -H "X-Scope-OrgID: <project>" \
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

If a separate workload needs log isolation, add a new tenant by:

1. Setting its writers to send `X-Scope-OrgID: <new-tenant>` instead of `<project>`.
2. Adding a per-tenant `limits_config` block in [`loki-values.yaml`](../../instance-template/apl-values/example/values.yaml) — see Loki's [multi-tenancy docs](https://grafana.com/docs/loki/latest/operations/multi-tenancy/) for ingestion-rate / retention overrides.
3. Adding a second Loki data source in Grafana for that tenant (header value differs).

Don't reuse `<project>` as a catch-all — once a workload's logs are mixed in there, splitting them out later is painful.

---

## SLA + rotation

The bucket-access key Loki uses to talk to Linode Object Storage is `secret/loki/object-store` in OpenBao, rotated declaratively by the `object-storage` Terraform module (`time_rotating`) on a 120-day cadence. Drift is monitored by the `loki-objkey-rotation-health` scheduled check (warns at 105d, errors at 120d) — see [`docs/runbooks/linode-credential-rotation.md`](../runbooks/linode-credential-rotation.md) for the manual reseed procedure if it ever falls behind.
