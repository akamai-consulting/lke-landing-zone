#!/usr/bin/env bash
# scaffold-render-check.sh — fast, LOCAL, no-cloud check of the per-ENV scaffold.
#
# Companion to template-scripts/ci/instance-test.sh. instance-test runs `copier copy`
# (template-token render) + `terraform validate` + actionlint — it catches HCL /
# module / workflow / token bugs, but it NEVER runs the scaffolder (llz env add), so
# the per-env scaffold is untested. And because `apl_values_env` has no default,
# `terraform validate` skips the cluster-bootstrap `templatefile(...)` call, so a
# bad `${...}` inside an apl-values/<env>/values.yaml is invisible to it.
#
# That gap is exactly where the e2e-only failures lived: an unfilled `your-env`
# placeholder in cluster-bootstrap/<env>.tfvars, a per-env file the bootstrap
# reads but the template never shipped (env-revision-configmap.yaml), and a
# literal `${...}` in a values.yaml comment that breaks templatefile(). Each cost
# a full ~20-min Release-E2E round-trip to discover. This check reproduces them
# in seconds with no cloud:
#
#   1. Scaffold a throwaway env via llz env add (the real path).
#   2. Assert no `your-env` placeholder survived in the generated tfvars/overlay.
#   3. Assert the per-env files cluster-bootstrap reads at plan time exist.
#   4+5. `llz ci validate-apl-values` on the rendered values.yaml: the
#      templatefile var-contract (every unescaped ${...} is a key in
#      cluster-bootstrap/main.tf's templatefile() map — the ${apl_values_repo_url}
#      class) AND apl-core's chart schema via `helm template apl/apl` (the
#      required/renamed/mistyped-key class, e.g. v6's `apps.loki: adminPassword
#      is required`). Both were previously Release-E2E-only.
#
# It does NOT stand up a cluster or run `terraform plan` (remote_state.cluster
# and the kubeconfig provider still need a real cluster — that stays Release-E2E
# / a long-lived dev cluster). All generated artifacts are removed on exit.
#
# Usage: template-scripts/ci/scaffold-render-check.sh
# Env:
#   SCAFFOLD_CHECK_ENV          throwaway env name   (default: scaffoldcheck)
#   SCAFFOLD_CHECK_REGION       Linode region        (default: us-ord)
#   SCAFFOLD_CHECK_OBJ_CLUSTER  OBJ cluster id       (default: us-ord-1)
#   (the schema half of step 4+5 self-skips when `helm` is absent)
set -euo pipefail

# shellcheck source=template-scripts/lib-common.sh
source "$(dirname "$0")/../lib-common.sh"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENV_NAME="${SCAFFOLD_CHECK_ENV:-scaffoldcheck}"
REGION="${SCAFFOLD_CHECK_REGION:-us-ord}"
OBJ_CLUSTER="${SCAFFOLD_CHECK_OBJ_CLUSTER:-us-ord-1}"

# `llz env add` is the scaffolder now (the bash new-deployment.sh was folded into
# the binary). Prefer a prebuilt bin/llz, an llz on PATH, else build one.
if [[ -n "${LLZ:-}" ]]; then :
elif [[ -x "$ROOT/bin/llz" ]]; then LLZ="$ROOT/bin/llz"
elif command -v llz >/dev/null 2>&1; then LLZ="$(command -v llz)"
else
  echo "Building llz (no bin/llz or llz on PATH)…" >&2
  ( cd "$ROOT/tools" && go build -o "$ROOT/bin/llz" ./cmd/llz )
  LLZ="$ROOT/bin/llz"
fi

# step() and fail() come from lib-common.sh; fail() accumulates into FAILED.
FAILED=0

INSTANCE="$ROOT/instance-template"
GEN_TFVARS=(
  "$INSTANCE/terraform-iac-bootstrap/cluster/$ENV_NAME.tfvars"
  "$INSTANCE/terraform-iac-bootstrap/object-storage/$ENV_NAME.tfvars"
)
GEN_OVERLAY="$INSTANCE/apl-values/$ENV_NAME"
ENV_YAML="$INSTANCE/environments/$ENV_NAME.yaml"   # spec ClusterDefinition `llz env add` authors
LZ="$INSTANCE/landingzone.yaml"                     # created on the first env add (untracked in the template)
TV="$ROOT/.template-version"   # llz env add stamps this at repo root

# Refuse to touch a real, tracked env of the same name.
for f in "${GEN_TFVARS[@]}" "$GEN_OVERLAY" "$ENV_YAML"; do
  if git -C "$ROOT" ls-files --error-unmatch "$f" >/dev/null 2>&1; then
    echo "::error::scaffold-check: '$ENV_NAME' is a real tracked env (${f#"$ROOT"/}). Set SCAFFOLD_CHECK_ENV to a throwaway name."
    exit 1
  fi
done

# Snapshot .template-version + landingzone.yaml so the throwaway scaffold's
# stamp / first-env bootstrap doesn't clobber a real local copy.
TV_BAK=""; [[ -f "$TV" ]] && { TV_BAK="$(mktemp)"; cp "$TV" "$TV_BAK"; }
LZ_BAK=""; [[ -f "$LZ" ]] && { LZ_BAK="$(mktemp)"; cp "$LZ" "$LZ_BAK"; }

cleanup() {
  rm -rf "${GEN_TFVARS[@]}" "$GEN_OVERLAY" "$ENV_YAML"
  if [[ -n "$TV_BAK" ]]; then mv -f "$TV_BAK" "$TV"; else rm -f "$TV"; fi
  if [[ -n "$LZ_BAK" ]]; then mv -f "$LZ_BAK" "$LZ"; else rm -f "$LZ"; fi
}
cleanup            # pre-clean leftovers from an interrupted prior run
TV_BAK=""; [[ -f "$TV" ]] && { TV_BAK="$(mktemp)"; cp "$TV" "$TV_BAK"; }
LZ_BAK=""; [[ -f "$LZ" ]] && { LZ_BAK="$(mktemp)"; cp "$LZ" "$LZ_BAK"; }
trap cleanup EXIT

# ── 1. Scaffold a throwaway env (the real `llz env add` path) ─────────────────
step "Scaffold throwaway env '$ENV_NAME' (region=$REGION obj=$OBJ_CLUSTER)"
# Run from $ROOT so llz's layout detection finds instance-template/.
if ! out="$( ( cd "$ROOT" && "$LLZ" env add "$ENV_NAME" --region "$REGION" --obj-cluster "$OBJ_CLUSTER" ) 2>&1)"; then
  printf '%s\n' "$out"
  fail "llz env add failed to scaffold '$ENV_NAME'"
  exit 1
fi
echo "scaffolded ${GEN_OVERLAY#"$ROOT"/} + 2 tfvars"

# ── 2. No unfilled placeholders ──────────────────────────────────────────────
step "Check for leftover 'your-env' placeholders"
# Comments legitimately mention "<your-env>" (e.g. the "Copy to <env>.tfvars"
# usage line); only an unfilled VALUE is a bug. Drop pure-comment matches
# (content after the file:line: prefix starting with '#').
hits="$(grep -rnH "your-env" "${GEN_TFVARS[@]}" "$GEN_OVERLAY" 2>/dev/null | grep -vE ':[0-9]+:[[:space:]]*#' || true)"
if [[ -n "$hits" ]]; then
  printf '%s\n' "$hits"
  fail "unfilled 'your-env' placeholder(s) above (comments excluded) — llz env add did not substitute them"
else
  echo "none (comments excluded)."
fi

# ── 3. Per-env files `llz ci bootstrap-cluster` reads ─────────────────────────
# Mirrors tools/cmd/llz/ci_bootstrap_cluster.go: it renders values.yaml and reads
# manifest/env-revision-configmap.yaml. Keep in sync if those reads change.
step "Check required per-env files exist"
REQUIRED=(
  "$GEN_OVERLAY/values.yaml"
  "$GEN_OVERLAY/manifest/env-revision-configmap.yaml"
)
for f in "${REQUIRED[@]}"; do
  if [[ -f "$f" ]]; then echo "ok   ${f#"$ROOT"/}"; else fail "missing required per-env file: ${f#"$ROOT"/}"; fi
done

# ── 4+5. runtime-placeholder var-contract + apl-core schema (unit-tested Go) ───
# `llz ci validate-apl-values` owns both checks (the logic lives in tested Go —
# ci_apl_schema.go — so this stays thin glue): (4) every unescaped ${...} left in
# the rendered values is one of the secrets-only runtime placeholders
# `llz ci bootstrap-cluster` fills (the ${apl_values_repo_url} class that failed a
# 2026-07-02 apply); (5) the values pass apl-core's chart schema via
# `helm template apl/apl`, pinned to the spec's aplChartVersion (the v6
# `apps.loki: adminPassword is required` class). The schema half self-skips
# without helm or a chart version; the var-contract half always runs.
step "Validate apl-values (runtime-placeholder var-contract + apl-core schema)"
if [[ -f "$GEN_OVERLAY/values.yaml" ]]; then
  # Best-effort chart version from the scaffolded spec; empty → the Go side
  # resolves the llz baseline (what an unpinned env actually deploys), so the
  # schema check runs either way. The leading `[[:space:]]*` anchor keeps a
  # COMMENTED-OUT `# aplChartVersion: 6.0.0` (the example's default state) from
  # being read as a real pin.
  CHART_VER="$(grep -hoE '^[[:space:]]*aplChartVersion:[[:space:]]*["'"'"']?[0-9]+\.[0-9]+\.[0-9]+' "$ENV_YAML" "$LZ" 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1 || true)"
  "$LLZ" ci validate-apl-values \
    --values "$GEN_OVERLAY/values.yaml" \
    --chart-version "$CHART_VER" \
    || fail "apl-values validation failed (see above) — fix before it fails at helm_release apl in Release-E2E"
fi

# ── 6. kustomize-build every rendered overlay (Argo's load-restrictor) ────────
# The blast-radius decomposition renders per-env apps/<name>/ source roots that pull
# in the shared kustomize Component three levels up + env patches. The render-golden
# Go tests assert STRINGS; only an actual kustomize build proves Argo can materialize
# the result — catching a broken Component ref, a missing/renamed patch file, or a
# load-restrictor escape BEFORE a ~40-min e2e. Build the manifest overlay + every
# apps/<name>/ with the SAME load restrictor Argo runs (LoadRestrictionsNone), which
# is why the ../ cross-dir refs resolve. kubectl ships kustomize; skip when absent
# (CI's scaffold job installs it).
step "kustomize-build the rendered overlays (LoadRestrictionsNone)"
if command -v kubectl >/dev/null 2>&1; then
  BUILD_DIRS=("$GEN_OVERLAY/manifest")
  if [[ -d "$GEN_OVERLAY/apps" ]]; then
    while IFS= read -r d; do BUILD_DIRS+=("$d"); done \
      < <(find "$GEN_OVERLAY/apps" -mindepth 1 -maxdepth 1 -type d | sort)
  fi
  for d in "${BUILD_DIRS[@]}"; do
    [[ -f "$d/kustomization.yaml" ]] || continue
    if err="$(kubectl kustomize "$d" --load-restrictor LoadRestrictionsNone 2>&1 >/dev/null)"; then
      echo "ok   build ${d#"$ROOT"/}"
    else
      printf '%s\n' "$err"
      fail "kustomize build failed: ${d#"$ROOT"/}"
    fi
  done
else
  echo "kubectl absent — skipping kustomize-build (CI's scaffold job enforces it)."
fi

# ── result ───────────────────────────────────────────────────────────────────
echo
if [[ "$FAILED" -ne 0 ]]; then
  echo "::error::scaffold-render-check FAILED"
  exit 1
fi
echo "scaffold-render-check OK"
