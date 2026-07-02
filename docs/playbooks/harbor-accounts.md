# Harbor Accounts — Playbook

**Applies to:** Harbor (`harbor.<primary-cluster>.internal:5000`), deployed on the primary cluster only. Other clusters consume Harbor remotely via `secret/harbor/pull-robot`.

**Related:** [`docs/runbooks/bootstrap-openbao.md`](../runbooks/bootstrap-openbao.md) (initial Harbor admin + robot bootstrap), `llz ci harbor-provisioner` ([`tools/cmd/llz/ci_harbor_provisioner.go`](../../tools/cmd/llz/ci_harbor_provisioner.go), canonical robot creation — the in-cluster harbor-robot-provisioner CronJob).

---

## Who needs what

| Principal | Type | How |
|---|---|---|
| Operator (you) | Human | Harbor UI login as `admin` with the Helm-generated password |
| CI build (the CI robot) | Machine | System robot — push/pull/delete on the `<project>` project |
| In-cluster image pull (`pull-<project>`) | Machine | System robot — pull-only on the `<project>` project |
| Anything else | Machine | New system robot, scoped to the minimum permissions it needs |

Harbor has no OIDC / LDAP integration in this deployment (`harbor-values.yaml`, managed by apl-core, does not enable `auth_mode=oidc_auth`). Human access is the local `admin` account; team members share that credential out-of-band, or you create individual local-DB users in the Harbor UI.

---

## Human account — UI login (recommended)

1. Get the admin password:

    ```bash
    kubectl -n registry get secret <release>-harbor-core \
      -o jsonpath='{.data.HARBOR_ADMIN_PASSWORD}' | base64 -d
    ```

    The same password is mirrored at `secret/harbor/admin` in OpenBao for ESO consumers — operators can also `llz openbao get active secret/harbor/admin password`.

2. Browse to `https://harbor.<primary-cluster>.internal:5000` (you must be on the cluster network / VPN). Log in as `admin`.

3. **For a per-person account** (preferred over shared admin) — in the UI, *Administration → Users → New User*:
   - Set a unique email and a strong password.
   - Add the user to the `<project>` project with the appropriate role (`Maintainer` for push, `Developer` for tag/scan, `Guest` for read-only).
   - Tell the user to log in at `https://harbor.<primary-cluster>.internal:5000`, change the initial password, and add their public SSH key under *User Profile* if they intend to use the Harbor CLI.

> **Don't** add new humans as Harbor system administrators unless they manage projects + robot accounts. The `<project>` project's per-project roles cover the normal operator surface.

---

## Machine account — system robot (CI / in-cluster)

Two robots already exist (the CI robot, `pull-<project>`) — both created by the in-cluster `harbor-robot-provisioner` CronJob (`llz ci harbor-provisioner`, [`tools/cmd/llz/ci_harbor_provisioner.go`](../../tools/cmd/llz/ci_harbor_provisioner.go)) once Harbor is up. To rotate one, delete the robot in Harbor UI — the next CronJob tick (~5m) recreates it, re-seeds OpenBao, and re-publishes the repo-level GitHub secrets. To add a new robot, run the same shape of API call by hand or extend that command.

### Adding a new robot by hand

```bash
# Auth as admin
HARBOR_PASS=$(kubectl -n registry get secret <release>-harbor-core \
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
  "https://harbor.<primary-cluster>.internal:5000/api/v2.0/robots" \
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
      registry_host="harbor.<primary-cluster>.internal:5000"
    ```

2. Add an `ExternalSecret` that syncs `secret/harbor/<my-robot>` into a Kubernetes Secret in the consuming namespace. Use an existing Harbor-pull ExternalSecret manifest as the template (same shape, different paths).

3. Reference the Secret from the workload's `imagePullSecrets` or app config.

4. Add the new OpenBao path to your ExternalSecret-path validation so the lint job covers the new ref.

---

## Rotation

- **Robot secrets**: delete the robot in the Harbor UI (*Administration → Robot Accounts*) and re-run `bootstrap-openbao.yml` (the script's robot-creation step returns 409 on existing robots and leaves them unchanged; deletion is the rotation trigger). The script will create a new robot with a new secret and re-seed OpenBao and the GitHub repo secrets.
- **Admin password**: rotate via the Harbor UI (*Administration → Users → admin → Change Password*), then re-seed `secret/harbor/admin` in OpenBao via `llz openbao set secret/harbor/admin password=<new>` so ESO stays in sync.
- **Human accounts**: standard Harbor UI password-reset flow.

---

## Removal

- **Robot**: *Administration → Robot Accounts → Delete*. Then `bao kv delete secret/harbor/<my-robot>` and remove the ExternalSecret manifest. Re-run the ExternalSecret-path validator to confirm clean.
- **Human user**: *Administration → Users → ... → Delete*. There is no OpenBao state to clean.
