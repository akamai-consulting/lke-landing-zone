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
#   4. Assert NO apl-core values.yaml was emitted. Linode owns apl-core's values
#      on the managed App Platform; a rendered one would be a file shipping to
#      instances that reaches no cluster, which is what happened to the retired
#      base for a year. This step used to run validate-apl-values (a
#      templatefile var-contract plus apl-core's chart schema via
#      `helm template apl/apl`) — that verb is retired, because the file it took
#      as input is no longer rendered.
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
# EVERY tfvars `llz env add` writes, or this script both under-checks and LEAKS:
# the array drives the placeholder scan AND the on-exit cleanup, so a root missing
# here leaves a scaffoldcheck.tfvars behind in the working tree after each run.
# That debris is not harmless — the template-manifest gate walks the FILESYSTEM
# (the CI container has no usable git), so leftover scaffold files make
# `make template-manifest-check` fail locally while CI, on a clean checkout, passes.
# Keep in step with instancelayout.Roots in
# tools/internal/shared/instancelayout/instancelayout.go, plus cluster-bootstrap.
GEN_TFVARS=(
  "$INSTANCE/terraform-iac-bootstrap/cluster/$ENV_NAME.tfvars"
  "$INSTANCE/terraform-iac-bootstrap/object-storage/$ENV_NAME.tfvars"
  "$INSTANCE/terraform-iac-bootstrap/databases/$ENV_NAME.tfvars"
  "$INSTANCE/terraform-iac-bootstrap/cluster-bootstrap/$ENV_NAME.tfvars"
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
  # `llz env add` also materializes each root's *.tf AND its .terraform.lock.hcl
  # from the embedded tfroots package. They are gitignored, regenerated on demand,
  # and were NOT being cleaned — so every run left ~6 files per root behind. That
  # is what makes `make template-manifest-check` fail on a developer's machine
  # while passing in CI: the gate walks the filesystem (the CI container has no
  # usable git) and counts this debris as unclassified scaffold.
  #
  # THE LOCK JOINED THE *.tf when it stopped being delivered. It used to be
  # tracked here and had to be spared; now it is generated beside them from the
  # same embed, so leaving it behind reintroduces exactly the debris this cleanup
  # exists for — and it is a DOTFILE, which `rm -f "$d"*.tf` would never have
  # matched even if the suffix had lined up.
  #
  # Iterate the root DIRECTORIES, not GEN_TFVARS: `vpc` is a root whose tfvars is
  # per-NETWORK (vpc/<name>.tfvars), not per-env, so it never appears in
  # GEN_TFVARS — and its six .tf files leaked from every run even after the
  # per-root cleanup landed. Globbing the roots also means the next root added is
  # cleaned without touching this script.
  for d in "$INSTANCE"/terraform-iac-bootstrap/*/; do rm -f "$d"*.tf "$d".terraform.lock.hcl; done
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
echo "scaffolded ${GEN_OVERLAY#"$ROOT"/} + ${#GEN_TFVARS[@]} tfvars"

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

# ── 3. Per-env files the platform-bootstrap Application syncs ─────────────────
# LLZ runs on the managed platform: apl-core (Linode) owns its own values.yaml, so
# `llz render` emits NO apl-core values.yaml — only the manifest/ tree the
# platform-bootstrap Application syncs. env-revision-configmap.yaml is the one
# per-env marker the bootstrap flow reads.
step "Check required per-env files exist"
REQUIRED=(
  "$GEN_OVERLAY/manifest/env-revision-configmap.yaml"
)
for f in "${REQUIRED[@]}"; do
  if [[ -f "$f" ]]; then echo "ok   ${f#"$ROOT"/}"; else fail "missing required per-env file: ${f#"$ROOT"/}"; fi
done
# The apl-core values.yaml must NOT be emitted on the managed platform.
if [[ -f "$GEN_OVERLAY/values.yaml" ]]; then
  fail "unexpected apl-core values.yaml at ${GEN_OVERLAY#"$ROOT"/}/values.yaml — managed apl-core owns its own values; LLZ must not render one"
fi

# ── 4. kustomize-build every rendered overlay (Argo's load-restrictor) ────────
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
