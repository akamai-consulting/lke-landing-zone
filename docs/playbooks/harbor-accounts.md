# Harbor Accounts — Playbook

**Applies to:** Harbor, deployed on the primary cluster only. Other clusters consume Harbor remotely via `secret/harbor/pull-robot`.

> **The registry host is discovered, never hand-assembled.** On Managed App Platform
> Linode owns the cluster domain (`lke<id>.akamai-apl.net`) and the spec validator
> *rejects* `cluster.bootstrap.domainSuffix`, so there is no domain in your spec to
> build a URL from. Read the authoritative host from the credential itself:
>
> ```bash
> HARBOR_HOST=$(llz openbao get active secret/harbor/pull-robot registry_host)
> ```
>
> (`llz ci resolve-harbor-url` resolves the same thing from a live cluster.) Every
> `$HARBOR_HOST` below is that value. A hand-built host that is wrong-but-non-empty
> defeats every empty-string fallback in the stack and surfaces as a 401 on both
> push and pull — it has cost a full debugging cycle before.

**Related:** [`docs/runbooks/bootstrap-openbao.md`](../runbooks/bootstrap-openbao.md) (initial Harbor admin + robot bootstrap), `llz ci harbor-provisioner` ([`tools/cmd/llz/ci_harbor_provisioner.go`](https://github.com/akamai-consulting/lke-landing-zone/blob/main/tools/cmd/llz/ci_harbor_provisioner.go), canonical robot creation — the in-cluster harbor-robot-provisioner CronJob).

---

## Who needs what

| Principal | Type | How |
|---|---|---|
| Operator (you) | Human | Harbor UI login as `admin` with the Helm-generated password |
| CI build (the CI robot) | Machine | System robot — push/pull/delete on the `<project>` project |
| In-cluster image pull (`pull-<project>`) | Machine | System robot — pull-only on the `<project>` project |
| Anything else | Machine | New system robot, scoped to the minimum permissions it needs |

Harbor has no OIDC / LDAP integration in this deployment (`harbor-values.yaml`, managed by apl-core, does not enable `auth_mode=oidc_auth`). Human access is the local `admin` account; team members share that credential out-of-band, or you create individual local-DB users in the Harbor UI.

### Which project is `<project>`?

`platform`. Two independent things decide this and they agree:

- **apl-core** creates Harbor projects per **APL team** — `team-<name>` for each team it provisions, plus the platform-services project `platform` and Harbor's own `library`. It does **not** create a project named after your instance, your repo, or a Kubernetes namespace. Pushing to a project that does not exist fails with a **401**, not a 404 — it reads like a credential problem and is not one.
- **The robot** the provisioner creates (`secret/harbor/robot`) is a *system* robot scoped to `platform`. It can push there and nowhere else.

So a workload that builds images pushes to `platform/<image>`. If you want a different project, create it **and** widen the robot's scope (see "Adding a new robot by hand" below) — both, or the push still 401s.

### Pulling images in your own namespace

Nothing distributes an imagePullSecret for you. `secret/harbor/pull-robot` holds the pull-only credential, and the platform builds a `harbor-docker-config` dockerconfigjson **only in the cert-automation namespace** — not in yours. The `platform` project is private, so a pod in your namespace pulling from it fails with `no basic auth credentials`.

Give your namespace its own pull secret with an ExternalSecret that renders the dockerconfigjson from the robot creds (the same shape `llz-cert-automation` uses — `username` / `password` / `registry_host` at `secret/harbor/pull-robot`), then reference it from the pod spec:

```yaml
spec:
  template:
    spec:
      imagePullSecrets:
        - name: harbor-pull
```

Note this reads a **platform** path, which the ESO store's platform allowlist already covers — unlike your own app secrets, which must live under a `spec.teams` subtree (see `kubernetes-custom/README.md`).

---

## Human account — UI login (recommended)

1. Get the admin password:

    ```bash
    kubectl -n harbor get secret harbor-admin-password \
      -o jsonpath='{.data.HARBOR_ADMIN_PASSWORD}' | base64 -d
    ```

    The same password is mirrored at `secret/harbor/admin` in OpenBao for ESO consumers — operators can also `llz openbao get active secret/harbor/admin password`.

2. Browse to `https://$HARBOR_HOST` (you must be on the cluster network / VPN). Log in as `admin`.

3. **For a per-person account** (preferred over shared admin) — in the UI, *Administration → Users → New User*:
   - Set a unique email and a strong password.
   - Add the user to the `<project>` project with the appropriate role (`Maintainer` for push, `Developer` for tag/scan, `Guest` for read-only).
   - Tell the user to log in at `https://$HARBOR_HOST`, change the initial password, and add their public SSH key under *User Profile* if they intend to use the Harbor CLI.

> **Don't** add new humans as Harbor system administrators unless they manage projects + robot accounts. The `<project>` project's per-project roles cover the normal operator surface.

---

## Machine account — system robot (CI / in-cluster)

Two robots already exist (the CI robot, `pull-<project>`) — both created by the in-cluster `harbor-robot-provisioner` CronJob (`llz ci harbor-provisioner`, [`tools/cmd/llz/ci_harbor_provisioner.go`](https://github.com/akamai-consulting/lke-landing-zone/blob/main/tools/cmd/llz/ci_harbor_provisioner.go)) once Harbor is up. The CronJob owns their whole lifecycle — see [Rotation](#rotation). To add a new robot, run the same shape of API call by hand or extend that command.

### Adding a new robot by hand

```bash
# The registry host (see the note at the top — never hand-assemble it)
HARBOR_HOST=$(llz openbao get active secret/harbor/pull-robot registry_host)

# Auth as admin
HARBOR_PASS=$(kubectl -n harbor get secret harbor-admin-password \
  -o jsonpath='{.data.HARBOR_ADMIN_PASSWORD}' | base64 -d)

# Decide the minimum permissions. Examples below — adjust to taste.
#   pull only:        [{"resource":"repository","action":"pull"}]
#   pull + push:      add {"resource":"repository","action":"push"}
#   pull + push + del: add {"resource":"repository","action":"delete"}
#   scan reports:     add {"resource":"scan","action":"read"}

curl -fsSL \
  -u "admin:${HARBOR_PASS}" \
  -H "Content-Type: application/json" \
  -X POST \
  "https://${HARBOR_HOST}/api/v2.0/robots" \
  -d '{
    "name": "<robot-name>",
    "description": "<purpose, owning team>",
    "duration": -1,
    "level": "system",
    "permissions": [{
      "kind": "project",
      "namespace": "<project>",
      "access": [
        {"resource": "repository", "action": "pull"}
      ]
    }]
  }'
```

The response body contains `.name` (the full robot name, prefixed with `robot$`) and `.secret` (shown **exactly once** — write it down before the response is closed). Both values are masked from logs by GitHub Actions when the bootstrap script seeds them; if you create a robot by hand, you are responsible for handling them safely.

**Field rules** (Harbor 2.x — the script's comments document these because they bit a previous bootstrap):

- `duration: -1` means "never expires." Harbor rejects the request with HTTP 400 if omitted.
- `level: "system"` is required to write to a project the robot doesn't directly own. Project-level robots are scoped narrower and can't authenticate from outside that project.
- `namespace: "<project>"` matches the existing project. If you create a new project, ensure it exists before creating the robot — the bootstrap script does this via `POST /api/v2.0/projects`.

### Wiring the new robot into the cluster

If the robot is consumed by an in-cluster workload:

1. Seed the credentials into OpenBao at a new path (mirror the existing layout):

    ```bash
    llz openbao set secret/harbor/<my-robot> \
      username="$ROBOT_NAME" \
      password="$ROBOT_SECRET" \
      registry_host="$HARBOR_HOST" \
      --yes
    ```

2. Add an `ExternalSecret` that syncs `secret/harbor/<my-robot>` into a Kubernetes Secret in the consuming namespace. Use an existing Harbor-pull ExternalSecret manifest as the template (same shape, different paths).

3. Reference the Secret from the workload's `imagePullSecrets` or app config.

4. Add the new OpenBao path to your ExternalSecret-path validation so the lint job covers the new ref.

---

## Rotation

- **Robot secrets**: delete the robot in the Harbor UI (*Administration → Robot
  Accounts*). Deletion is the whole trigger — the in-cluster
  `harbor-robot-provisioner` CronJob recreates it on its next tick (~5m), re-seeds
  OpenBao, and re-publishes the repo-level `HARBOR_*` GitHub secrets. Creation is
  409-on-existing, so nothing rotates until you delete.

  > Do **not** re-run `bootstrap-openbao.yml` for this. The provisioner moved
  > in-cluster; on an active/standalone cluster the workflow no longer creates
  > robots, so a bootstrap run is a slow no-op for this purpose. (It still seeds a
  > **standby** peer from the active's published secrets — that is a different job.)

- **Admin password**: rotate via the Harbor UI (*Administration → Users → admin →
  Change Password*), then re-seed OpenBao so ESO stays in sync. **`--yes` is
  required** — without it the command prints a plan, writes nothing, and exits 0,
  leaving the password rotated in Harbor and stale in OpenBao:

  ```bash
  llz openbao set secret/harbor/admin password=<new> --yes
  llz openbao get active secret/harbor/admin password    # verify it landed
  ```

- **Human accounts**: standard Harbor UI password-reset flow.

---

## Removal

- **Robot**: *Administration → Robot Accounts → Delete*. Then destroy the OpenBao
  copy and remove the ExternalSecret manifest:

  ```bash
  # `bao kv delete` only SOFT-deletes the latest version — the secret material
  # stays readable by version. Destroying the metadata is what removes it.
  llz openbao exec -- kv metadata delete secret/harbor/<my-robot>
  ```

  Re-run the ExternalSecret-path validator to confirm clean.
- **Human user**: *Administration → Users → ... → Delete*. There is no OpenBao state to clean.
