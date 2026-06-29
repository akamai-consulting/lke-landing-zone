#!/usr/bin/env bash
# Reusable Rust/Spin quality gates for the akamai-functions kit. App-agnostic: every
# target runs over the WHOLE cargo workspace, so it covers whatever crates your app ships
# — no hardcoded crate paths. Run locally (./scripts/app/quality.sh coverage) or via the
# scaffolded CI matrix. Thresholds are env-tunable.
set -euo pipefail
COV_MIN="${COV_MIN:-90}"
MUTANTS_TIMEOUT="${MUTANTS_TIMEOUT:-120}"
SEMVER_BASELINE="${SEMVER_BASELINE:-origin/main}"

run() { echo "+ $*" >&2; "$@"; }

case "${1:-}" in
  fmt-check)  run cargo fmt --all -- --check ;;
  clippy)     run cargo clippy --workspace --all-targets -- -D warnings ;;
  test)       run cargo test --workspace ;;
  coverage)   run cargo tarpaulin --workspace --engine llvm --out Xml \
                  --output-dir coverage --fail-under "$COV_MIN" ;;
  mutants)    run cargo mutants --workspace --timeout "$MUTANTS_TIMEOUT" ;;
  audit)      run cargo audit ;;
  deny)       run cargo deny check ;;
  machete)    run cargo machete ;;
  semver)     run cargo semver-checks --workspace --baseline-rev "$SEMVER_BASELINE" ;;
  shellcheck) files=$(git ls-files '*.sh'); [ -n "$files" ] && run shellcheck $files || echo "no shell scripts" ;;
  all)        for t in fmt-check clippy test coverage audit deny machete semver shellcheck mutants; do "$0" "$t"; done ;;
  *) echo "usage: quality.sh {fmt-check|clippy|test|coverage|mutants|audit|deny|machete|semver|shellcheck|all}" >&2; exit 2 ;;
esac
