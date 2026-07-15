# Extending `llz`

`llz` is the front-end for your LKE landing-zone instance. Beyond the built-in
commands it ships, each instance can add its **own** subcommands and its own
pre-commit checks — without forking the template or fighting `copier update`.
Both extension points are `owned` files (see `.template-manifest`): a template
update never touches them.

## Your own subcommands — `.llz/commands.yaml`

Drop a `.llz/commands.yaml` at the instance root. `llz` reads it at startup and
registers each entry as a real subcommand — it shows up in `llz --help` and gets
shell completion.

```yaml
# .llz/commands.yaml — operator-owned; commit it.
commands:
  - name: smoke
    short: run the instance smoke test
    argv: [bash, hack/smoke.sh]

  - name: psql
    short: open a psql shell against the cluster db
    argv: [./hack/psql.sh]

  - name: tf-plan
    short: terraform plan the cluster root
    argv: [tofu, -chdir=terraform/cluster, plan]
```

Now:

```bash
llz smoke                 # runs: bash hack/smoke.sh
llz psql --db readonly    # extra args are appended to argv: ./hack/psql.sh --db readonly
```

### Schema

| field   | required | meaning                                                            |
| ------- | -------- | ------------------------------------------------------------------ |
| `name`  | yes      | the subcommand name (`llz <name>`)                                 |
| `argv`  | yes      | the command to run, as a list (`argv[0]` is the executable)        |
| `short` | no       | one-line help shown in `llz --help`                                |

### Behavior & rules

- **Arg passthrough.** Everything you type after the command name is appended to
  `argv` verbatim, so a command behaves like a smart alias — flags included. This
  mirrors the built-in `drift` / `env add` commands. Because flag parsing is off,
  `llz`'s own flags (like `--dry-run`) are **not** applied to these commands; rely
  on your command's own flags instead.
- **Built-ins win.** An entry whose `name` collides with a built-in command (e.g.
  `lint`, `build`, `tokens`) is skipped with a warning — you can't shadow them.
- **Malformed entries are skipped** (empty `name` or `argv`) with a warning, not a
  hard failure, so one bad entry doesn't break `llz`.
- **Relative paths** resolve against the directory you run `llz` from (the
  instance root, normally).

This replaces the old `Makefile.local` escape hatch.

## Your own Kubernetes resources — `apl-values/_shared/custom/`

Need to apply your own manifests to the cluster — a NetworkPolicy, a ConfigMap,
an app Deployment, an ExternalSecret, or a whole Helm chart? Drop them in
`apl-values/_shared/custom/`. **No Terraform, no edits to the LLZ-managed
bootstrap tree.** Like the hatches above, this directory is `owned` (see
`.template-manifest`): the template ships it once and a `copier update` never
touches it again, and `llz render` never overwrites it.

Argo CD applies whatever you put there via a dedicated `instance-custom`
Application. It syncs at **sync-wave 10** — after the platform support plane is
healthy — so your resources can rely on cert-manager, External Secrets + the
`openbao` ClusterSecretStore, namespaces, and the default-deny NetworkPolicies
already being up.

The directory is a **kustomize root**. Edit its `kustomization.yaml`:

### Raw manifests / kustomize

```yaml
# apl-values/_shared/custom/kustomization.yaml — owned; commit it.
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - my-namespace.yaml
  - my-app-deployment.yaml
  - my-networkpolicy.yaml
  - my-externalsecret.yaml
```

### Helm / OCI charts

Drop in your own Argo CD `Application` pointing at a chart and list it under
`resources:`. It rides the permissive `instance-custom` AppProject, so any chart
repo works. **Pin the chart version in your Application** — that's your source of
truth:

```yaml
# apl-values/_shared/custom/apps/my-helm-app.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: { name: my-helm-app, namespace: argocd }
spec:
  project: instance-custom
  source:
    repoURL: <your chart repo>
    chart: <chart>
    targetRevision: <pinned version>
  destination: { server: https://kubernetes.default.svc, namespace: my-app }
  syncPolicy: { automated: { prune: true, selfHeal: true } }
```

### Behavior & rules

- **Isolated blast radius.** A broken manifest here degrades only the
  `instance-custom` Application — it **cannot** wedge the platform bootstrap. The
  app-of-apps only syncs the always-valid `instance-custom` Application object;
  your content is health-gated by `instance-custom` itself.
- **Wide-open AppProject by design.** `instance-custom` allows any source repo,
  any namespace, and any resource kind (including `Application`, so you can run
  your own app-of-apps). It's an escape hatch — the trust boundary is the cluster
  edge, not the project. Tighten `sourceRepos` / `destinations` in
  `platform-apl/manifest/instance-custom-project.yaml` if you want a
  narrower scope (that file is template-managed, so re-apply on update).
- **`prune: false`.** Your resources are yours to remove deliberately; an
  accidental empty render won't cascade-delete them.
- **Revision.** The Application tracks the values repo's default branch (`HEAD`).
  If you pin `apps_repo_revision` to a tag/sha, pin the Application's
  `targetRevision` in `applications/instance-custom.yaml` to match.

## Extra pre-commit checks — `.githooks/pre-commit.local`

The pre-commit hook (installed by `llz hooks`, see below) runs a secrets guard +
`llz lint`. To add your own checks, drop an **executable** script at
`.githooks/pre-commit.local`:

```bash
#!/usr/bin/env bash
# .githooks/pre-commit.local — runs after `llz lint`. Non-zero exit blocks the commit.
set -euo pipefail
./hack/check-no-todo-markers.sh
```

```bash
chmod +x .githooks/pre-commit.local
```

`llz precommit` runs it after the built-in lint gate; a non-zero exit blocks the
commit. It's `owned`, so updates never overwrite it.

## The built-in checks

The checks that used to live in the instance `Makefile` are now part of `llz`, so
they ship with the binary (upgrade `llz` to get fixes) rather than via `copier
update`:

| command            | does                                                          |
| ------------------ | ------------------------------------------------------------ |
| `llz lint`         | fast gate: `tofu fmt -check` + `tflint` + `actionlint` + `gitleaks` |
| `llz fmt`          | auto-fix: `tofu fmt`                                          |
| `llz validate`     | heavier: `terraform validate` + `checkov`                    |
| `llz check <step>` | advanced/debug hatch (hidden from top-level help): run one step in isolation (`fmt-check`, `tf-lint`, `actions-lint`, `gitleaks`, `tf-validate`, `checkov`) |

A missing tool **skips with a warning** instead of blocking, so an absent linter
never wedges a commit. Each tool's binary can be overridden via an env var
(`LLZ_TOFU`, `LLZ_TFLINT`, `LLZ_ACTIONLINT`, `LLZ_GITLEAKS`, `LLZ_TERRAFORM`,
`LLZ_CHECKOV`) — the equivalent of the Makefile's `TOFU ?= …` overrides. The lint
configs they read (`.tflintrc.hcl`, `.checkov.yaml`, `.gitleaks.toml`) are
template-`managed` files that `copier update` keeps current.

## The pre-commit hook — `llz hooks`

`llz hooks` installs the pre-commit hook into the repo's git hooks directory
(`.git/hooks/pre-commit`). The hook is a small shim that `exec`s `llz` by the
**absolute path** of the binary that installed it (with a `$PATH` fallback) — so
commits run the checks even when `llz` isn't on `$PATH`.

```bash
llz hooks            # install / re-install
llz --dry-run hooks  # show what it would write
```

Because the hook lives under `.git/` it is **not committed** and is per-clone:

- `llz new` and `copier copy --trust` arm it automatically on scaffold.
- After a fresh `git clone` of an existing instance, run `llz hooks` once.
