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
