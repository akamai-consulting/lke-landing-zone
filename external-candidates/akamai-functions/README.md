# akamai-functions (external candidate)

The reusable **Spin → Akamai Fermyon Wasm Functions** delivery kit + the standard **Rust
quality bar**. App-agnostic: it ships the CI pipeline, deploy action/script, toolchain,
and Fermyon secret — but no workload.

## What it scaffolds

- `.github/workflows/akamai-functions.yml` — quality matrix → build → deploy.
- `scripts/app/quality.sh` — the reusable gates, each over the whole workspace:
  `fmt-check`, `clippy`, `test`, **`coverage`** (cargo-tarpaulin, `--fail-under 90`),
  **`mutants`** (cargo-mutants — mutation testing), `audit`/`deny`/`machete` (supply
  chain), `semver` (cargo-semver-checks), `shellcheck`.
- `mutants.toml`, `deny.toml` — starter quality configs.
- `.github/actions/spin-cloud-deploy/`, `scripts/app/deploy-cloud.sh` — the deploy.

Bring your own Spin app and point `SPIN_MANIFEST` at its `spin.toml`. Run a gate locally
with `./scripts/app/quality.sh coverage`; thresholds are env-tunable (`COV_MIN`,
`MUTANTS_TIMEOUT`). Workload alerts/dashboards: `llz extension new --kind observability`.

## Try it / spin it out

    llz extension lint  external-candidates/akamai-functions
    llz extension apply external-candidates/akamai-functions --root /path/to/instance --dry-run

Push to its own repo; consume as a pinned remote source (gated enable). The quality
toolchain installs via `llz extension provision` (mise); `llz doctor` flags any missing.
