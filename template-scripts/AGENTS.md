# scripts/ — bash + python utilities

Shell + Python helpers for the landing-zone template. The heavy
per-environment operational logic (bootstrap, cluster health, openbao ops,
terraform orchestration, app deploy) lives in the llz CLI
([../tools/cmd/llz/](../tools/cmd/llz/) — the former repo-root
`instance-scripts/` were assimilated into it). It is template-INTERNAL (like
`.github/actions`): the reusable `llz-*` workflows check the template out into
`_llz-template/` and build llz from there (the install-llz action), so
instances never carry their own copy (see `copier.yml`). The scripts here are
the template-level scaffold + CI/lint helpers, and never leave the template repo.

## Layout

### top-level
- `stamp-template-version.sh` — write the committed `.template-version` provenance
  stamp. Still used by release-e2e (which passes explicit --repo/--sha/--ref for
  the throwaway instance); the operator path stamps natively via `llz`.
- `check-template-manifest.sh` — validate `.template-manifest` covers every
  scaffold file (CI gate) + classify a path on demand.
- `lib-common.sh` — shared helpers (`die`/`step`/`fail` logging, `usage`
  header-comment help printer, `detect_tf` terraform/tofu discovery,
  `install_release_tarball`); sourced via a relative path from the bucket
  scripts.

> Env scaffolding (the old `new-deployment.sh`) is now folded into the `llz`
> binary as `llz env add`, so it works inside a rendered instance (which carries
> no scripts/ tree). The Go implementation is layout-aware — run it in a template
> checkout and it writes into `instance-template/` exactly as the bash did.

### `ci/` — CI / runner infrastructure
GitHub Actions-side helpers + tooling install. Don't run these from a workstation.
- `install-syft.sh` / `install-trivy.sh` — pinned SBOM/scanner installers
- `sbom-kubernetes.sh` — extracts images from cluster state and runs `syft` on each

### `linting-and-validation/` — CI lint inputs
- `cleanup-workflow-runs.sh` — deletes old GitHub Actions runs (maintenance utility)

> Rendered-app and Chart.lock validation moved into the unit-tested `llz` CLI:
> `llz ci argocd-rendered-apps` (duplicate Helm parameter names) and
> `llz ci chart-lock-drift` (Chart.lock vs Chart.yaml dependencies). The Go
> per-package coverage floor (`llz ci check-coverage`) likewise replaced
> `ci/check-go-coverage.sh`. The PrometheusRule promtool gate moved too:
> `llz ci check-prom-rules` replaced `check-prometheus-rule-crds.py`, retiring
> the last first-party Python script in the repo.

### `hooks/` — git hooks
Enable with `git config core.hooksPath scripts/hooks`. `pre-commit` blocks secret
files; `pre-push` builds the Go tools module and runs the lint gate.

## Conventions

- Use `#!/usr/bin/env bash` and `set -euo pipefail` at the top of every script.
- Resolve `SCRIPT_DIR` via `cd "$(dirname "$0")" && pwd`. Bucket scripts derive
  `WORKSPACE_ROOT="$SCRIPT_DIR/../.."`; top-level scripts use `"$SCRIPT_DIR/.."`.
  Never assume `$PWD` is the repo root.
- Source `lib-common.sh` by relative path (`"$SCRIPT_DIR/../lib-common.sh"`).
- Scripts may call `go`, `curl`, `jq`, `openssl`, `base64`, `kubectl`,
  `terraform`/`tofu`, `helm`, `gh`.
- Print every destructive step before running it. Never `rm -rf` a user-supplied
  path without a guarded prefix check.

## Rules

- Never add `Co-Authored-By` to commits.
- Do not add `--no-verify` or bypass git hooks.
- Do not echo secret contents (keys, tokens, seeds) to stdout. Use `>/dev/null`
  or `read -s`.
- Do not make changes without explicit user approval.
