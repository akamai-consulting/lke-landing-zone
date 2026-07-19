# Grafana Access — Playbook

**Applies to:** Grafana (`<release>-grafana` Deployment in the `grafana` namespace) on every cluster.

**Related:** your observability configuration (dashboards, metrics topology), [`grafana-values.yaml`](../../instance-template/apl-values/values.yaml).

---

## How access works

Grafana is **not exposed externally** — `service.type: ClusterIP` in `grafana-values.yaml`. There is no Ingress. The only way in is `kubectl port-forward` after you have a kubeconfig for the target cluster.

Auth is the Helm-chart-default local-DB admin user. The password is generated at first deploy and stored at `secret/grafana/admin` in OpenBao (synced into the `<release>-grafana-admin-credentials` Kubernetes Secret via ESO). There is no OIDC / LDAP — all access is the shared admin or per-person users you create in the Grafana UI.

---

## Operator access — port-forward

```bash
# 1. Make sure your kubeconfig points at the cluster you want to inspect.
#    The primary cluster is the canonical place to look — every cluster has the
#    same dashboards but different metrics.

# 2. Port-forward Grafana to localhost:3000
kubectl -n grafana port-forward svc/<release>-grafana 3000:80

# 3. Browse to http://localhost:3000

# 4. Get the admin password (from OpenBao — canonical) ...
llz openbao get active secret/grafana/admin password

# ... or from the in-cluster Secret (already synced by ESO):
kubectl -n grafana get secret <release>-grafana-admin-credentials \
  -o jsonpath='{.data.admin-password}' | base64 -d

# 5. Log in as admin / <password>
```

Dashboards are auto-loaded from ConfigMaps with label `grafana_dashboard=1` — you don't need to import anything manually.

---

## Per-person accounts (recommended for shared use)

Sharing the admin password works for one-off debugging but doesn't scale. To add a named user:

1. Log in as admin (see above).
2. *Administration → Users → New user*.
3. Set name, email, login, and an initial password. Mark the user as a Grafana admin only if they need to configure data sources or other admins; for read access, pick `Viewer`; for ad-hoc dashboard edits, `Editor`.
4. Tell the user to log in and change their initial password on first use.

These users live in Grafana's local SQLite DB — they survive pod restarts but **not** a full reinstall of the chart. If the Helm release is wiped, only the admin (sourced from OpenBao) survives.

---

## Adding a new data source

The two data sources today are Prometheus and Loki, both configured declaratively in [`grafana-values.yaml`](../../instance-template/apl-values/values.yaml). For a new data source:

1. Add a block under `grafana.datasources['datasources.yaml'].datasources` in `grafana-values.yaml`. Use the existing entries as a template.
2. If the data source needs auth: seed credentials in OpenBao, add the path to the `platform-ci` policy in [`llz ci bao-configure`](../../tools/cmd/llz/ci_openbao_configure.go), render an ExternalSecret, and reference the resulting Kubernetes Secret from the Grafana data source via `secureJsonData`.
3. PR + ArgoCD sync — the change applies on the next reconciliation.

Do not add data sources via the Grafana UI for production — they live only in the SQLite DB and disappear on reinstall.

---

## Adding a new dashboard

Drop a `.json` export into your apl-values dashboards directory and commit. ArgoCD creates the ConfigMap on the next sync; the Grafana sidecar picks it up within ~1 minute.

---

## Rotating the admin password

1. In Grafana UI: *Administration → Users → admin → Change Password*.
2. Re-seed OpenBao so ESO stays in sync:

    ```bash
    llz openbao set secret/grafana/admin password=<new-password>
    ```

3. Force ESO to refresh (optional — happens within `refreshInterval` otherwise):

    ```bash
    kubectl -n grafana annotate externalsecret grafana-admin-credentials \
      force-sync=$(date +%s) --overwrite
    ```

---

## If port-forward isn't enough

If you frequently need Grafana access and `kubectl port-forward` is too friction-heavy, the right next step is adding an Ingress + OIDC. That's out of scope for this playbook — track it under a separate design discussion (the existing posture is deliberate: no public Grafana surface).
