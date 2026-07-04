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
#   4. Render every apl-values/<env>/values.yaml through templatefile() with the
#      variable set DERIVED from cluster-bootstrap/main.tf — catching ${...} parse
#      errors and references to variables the root does not provide.
#   5. Validate the rendered values.yaml against apl-core's bundled schema via
#      `helm template apl/apl` (the same JSON-Schema check helm_release.apl runs
#      at apply time) — catching required/renamed/mistyped keys that otherwise
#      only surface at `terraform apply` in Release-E2E (the apl-core v6 migration
#      burned multiple runs on `apps.loki: adminPassword is required`).
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
#   TF                          terraform binary     (default: terraform, then tofu)
#   SKIP_TF                     set to 1 to skip the templatefile render step
#   (step 5 self-skips when `helm` is absent)
set -euo pipefail

# shellcheck source=template-scripts/lib-common.sh
source "$(dirname "$0")/../lib-common.sh"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENV_NAME="${SCAFFOLD_CHECK_ENV:-scaffoldcheck}"
REGION="${SCAFFOLD_CHECK_REGION:-us-ord}"
OBJ_CLUSTER="${SCAFFOLD_CHECK_OBJ_CLUSTER:-us-ord-1}"

# Empty when neither terraform nor tofu is present — the render step below skips.
TF="$(detect_tf)"

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
  "$INSTANCE/terraform-iac-bootstrap/cluster-bootstrap/$ENV_NAME.tfvars"
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
echo "scaffolded ${GEN_OVERLAY#"$ROOT"/} + 3 tfvars"

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

# ── 3. Per-env files cluster-bootstrap reads at plan time ─────────────────────
# Mirrors terraform-iac-bootstrap/cluster-bootstrap/main.tf: templatefile(values.yaml) and
# data.local_file.env_revision_configmap. Keep in sync if those reads change.
step "Check required per-env files exist"
REQUIRED=(
  "$GEN_OVERLAY/values.yaml"
  "$GEN_OVERLAY/manifest/env-revision-configmap.yaml"
)
for f in "${REQUIRED[@]}"; do
  if [[ -f "$f" ]]; then echo "ok   ${f#"$ROOT"/}"; else fail "missing required per-env file: ${f#"$ROOT"/}"; fi
done

# The templatefile() var set is DERIVED from cluster-bootstrap/main.tf, not
# hand-copied — a hardcoded duplicate drifts, and the failure mode is nasty in
# both directions: a stale SUPERSET masks an unresolved placeholder (the old
# 17-key map happily rendered ${apl_values_repo_url} after the spec render
# missed it → a Release-E2E plan failure), while a stale SUBSET false-fails a
# legitimately-added runtime var. Extracting the keys from the actual
# templatefile(...) block keeps this check honest for free.
MAIN_TF="$ROOT/instance-template/terraform-iac-bootstrap/cluster-bootstrap/main.tf"
# Slice the `apl_rendered_values = templatefile( … )` block (open paren line to
# the lone closing-paren line), drop the templatefile( line itself, then take
# the `<key> =` LHS of each map entry. Portable sed/grep — no gawk match(,arr).
tf_keys="$(sed -n '/apl_rendered_values[[:space:]]*=[[:space:]]*templatefile(/,/^[[:space:]]*)[[:space:]]*$/p' "$MAIN_TF" \
  | grep -v 'templatefile(' \
  | grep -oE '^[[:space:]]+[A-Za-z_][A-Za-z0-9_]*[[:space:]]*=' \
  | sed -E 's/[[:space:]=]//g' || true)"

# ── 4. templatefile() render (mirrors cluster-bootstrap locals.apl_rendered_values) ─
step "Render apl-values/$ENV_NAME/values.yaml via templatefile()"
if [[ -z "$TF" || "${SKIP_TF:-0}" == "1" ]]; then
  echo "(no terraform/tofu binary or SKIP_TF=1 — skipping render)"
elif [[ -z "$tf_keys" ]]; then
  fail "could not extract the templatefile() var set from ${MAIN_TF#"$ROOT"/} — the apl_rendered_values block was reformatted; fix the sed slice above"
elif [[ -f "$GEN_OVERLAY/values.yaml" ]]; then
  echo "templatefile() vars (from main.tf): $(echo "$tf_keys" | tr '\n' ' ')"
  vars="{"; for k in $tf_keys; do vars+="${k}=\"x\","; done; vars="${vars%,}}"
  tmp="$(mktemp -d)"
  printf 'terraform {}\n' > "$tmp/main.tf"
  ( cd "$tmp" && "$TF" init -backend=false >/dev/null 2>&1 ) || true
  if echo "length(templatefile(\"$GEN_OVERLAY/values.yaml\", $vars))" \
       | ( cd "$tmp" && "$TF" console ) >/dev/null 2>"$tmp/err"; then
    echo "rendered ok"
  else
    sed 's/^/    /' "$tmp/err" || true
    fail "templatefile() failed to render apl-values/$ENV_NAME/values.yaml (see above)"
  fi
  rm -rf "$tmp"
fi

# ── 5. apl-core values-schema validation (helm client-side JSON Schema) ────────
# The rendered values.yaml is the exact file cluster-bootstrap feeds to
# `helm_release.apl`, which validates it against apl-core's bundled
# values.schema.json BEFORE anything touches the cluster. That validation was
# e2e-ONLY: the apl-core v6 migration burned multiple Release-E2E runs on
# `apps.loki: adminPassword is required` (a new required key) surfacing at
# `terraform apply` time. `helm template` runs the identical schema check
# client-side (no cluster, no chart deps — apl/apl has none), so we run it here
# on the same rendered output. Stub the secret ${...} placeholders first (the
# schema checks structure/required-keys/types, not secret values); the chart
# version is read from the scaffolded tfvars so it always matches what deploys.
step "Validate rendered values against apl-core's schema (helm template)"
APL_VER="$(grep -oE 'apl_chart_version[[:space:]]*=[[:space:]]*"[0-9]+\.[0-9]+\.[0-9]+"' \
  "$ROOT/instance-template/terraform-iac-bootstrap/cluster-bootstrap/$ENV_NAME.tfvars" 2>/dev/null \
  | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true)"
if ! command -v helm >/dev/null 2>&1; then
  echo "(no helm binary — skipping apl-core schema validation)"
elif [[ -z "$APL_VER" ]]; then
  fail "could not read apl_chart_version from the scaffolded $ENV_NAME.tfvars — cannot pin the schema check"
elif [[ -f "$GEN_OVERLAY/values.yaml" ]]; then
  echo "apl chart version (from tfvars): $APL_VER"
  helm repo add apl https://linode.github.io/apl-core >/dev/null 2>&1 || true
  helm repo update apl >/dev/null 2>&1 || true
  tmp="$(mktemp -d)"
  # Stub every remaining ${...} placeholder — the leftover runtime secrets — with
  # a non-empty dummy so pattern/required checks see a present value. Real spec
  # values (URLs, domains, buckets) were already resolved by llz render.
  sed -E 's/\$\{[a-zA-Z_]+\}/dummy/g' "$GEN_OVERLAY/values.yaml" > "$tmp/values.yaml"
  if helm template apl apl/apl --version "$APL_VER" -f "$tmp/values.yaml" >/dev/null 2>"$tmp/err"; then
    echo "schema ok (apl/apl $APL_VER)"
  else
    sed 's/^/    /' "$tmp/err" || true
    fail "rendered values.yaml violates apl-core's schema (apl/apl $APL_VER) — see above; fix apl-values before it fails at helm_release.apl in Release-E2E"
  fi
  rm -rf "$tmp"
fi

# ── result ───────────────────────────────────────────────────────────────────
echo
if [[ "$FAILED" -ne 0 ]]; then
  echo "::error::scaffold-render-check FAILED"
  exit 1
fi
echo "scaffold-render-check OK"
