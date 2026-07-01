#!/usr/bin/env bash
# instance-test.sh — Fast, LOCAL, no-cloud smoke test of a template instance.
#
# This is the cheap local counterpart to .github/workflows/release-e2e.yml. That
# workflow proves the template end-to-end by standing up a REAL LKE-Enterprise
# cluster (instantiate → provision → validate → destroy) — slow and billable. Most
# instantiation bugs, though, surface with no cloud at all: a token that fails to
# render, a file that doesn't get copied, a Terraform root whose module wiring or
# types broke. This target catches those in ~a minute so you don't burn a full e2e
# run to learn the scaffold doesn't even render.
#
# What it does:
#   1. Instantiate — `copier copy` instance-template/ into a throwaway build dir.
#      This is the REAL instantiation path (the same one an adopter runs), so it
#      exercises the <@ token @> substitution that release-e2e's raw-`cp` hoist
#      silently skips (see the NOTE in release-e2e.yml's "Lay out as instance").
#      The working tree is included (copier's DirtyLocalWarning) so you test the
#      changes in front of you, not just the last commit.
#   2. Token residue — fail on any unrendered copier delimiter left in the output
#      (<@ @>, <% %>, <# #>). A leftover token = a substitution that didn't fire.
#   3. Structure — assert the load-bearing files actually rendered (answers file,
#      the _tasks-copied docs/, every Terraform root, the instance workflows) and
#      that scripts/ is NOT delivered (sourced from the template checkout instead).
#   4. Terraform validate — rewrite each rendered root's published git:: module
#      source to the in-repo terraform-modules/ path (same trick as
#      template-scripts/ci/instantiate-terraform.sh), then `init -backend=false` +
#      `validate` — HCL syntax, types, and module wiring with no tags/state/creds.
#   5. actionlint — lint the rendered instance workflows if actionlint is present
#      (a broken token render often shows up as invalid workflow YAML).
#
# It does NOT stand up a cluster or apply anything. release-e2e.yml remains the
# authoritative provision/validate/destroy gate.
#
# Usage: template-scripts/ci/instance-test.sh
# Env:
#   INSTANCE_TEST_DIR  build dir (default: .instance-test; gitignored)
#   TF                 terraform binary (default: terraform, falling back to tofu)
#   SKIP_TF            set to 1 to skip the terraform validate stage (no TF binary)
set -euo pipefail

# shellcheck source=template-scripts/lib-common.sh
source "$(dirname "$0")/../lib-common.sh"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUILD="${INSTANCE_TEST_DIR:-.instance-test}"
# Absolute so the git:: → local rewrite below resolves regardless of build depth.
case "$BUILD" in /*) : ;; *) BUILD="$ROOT/$BUILD" ;; esac
INSTANCE="$BUILD/instance"

# terraform proper in CI; locally this repo usually has only tofu (the dev alias).
# Default to `terraform` when neither is found — the validate stage below reports
# the missing binary and skips rather than aborting the whole run.
TF="$(detect_tf)"; TF="${TF:-terraform}"

# `llz env add` is the scaffolder (built into the binary). Resolve it so we can
# prove it works INSIDE a rendered instance (the instance carries no scripts/).
if [[ -n "${LLZ:-}" ]]; then :
elif [[ -x "$ROOT/bin/llz" ]]; then LLZ="$ROOT/bin/llz"
elif command -v llz >/dev/null 2>&1; then LLZ="$(command -v llz)"
else ( cd "$ROOT/tools" && go build -o "$ROOT/bin/llz" ./cmd/llz ) && LLZ="$ROOT/bin/llz"; fi

# step() and fail() come from lib-common.sh; fail() accumulates into FAILED.
FAILED=0

# ── 1. Instantiate via copier ─────────────────────────────────────────────────
step "Instantiate (copier copy → ${INSTANCE#"$ROOT"/})"
command -v copier >/dev/null 2>&1 || {
  echo "copier not found — install with: pipx install copier (or pip install --user copier)" >&2
  exit 1
}
rm -rf "$BUILD"
mkdir -p "$BUILD"
# --vcs-ref HEAD + the working tree (DirtyLocalWarning) so local edits are tested.
# --trust runs copier.yml _tasks (copies the operator docs/ into the instance).
# llz_version is fail-closed (copier.yml validator rejects an unpinned main/HEAD),
# so pass the latest published v* tag explicitly — the same concrete pin `llz new`
# renders. The validate stage below rewrites git:: sources to in-repo paths, so the
# exact tag value is not resolved over the network here.
LLZ_VERSION="$(git -C "$ROOT" describe --tags --match 'v[0-9]*' --abbrev=0 2>/dev/null || echo v0.0.0)"
copier copy --trust --defaults --vcs-ref HEAD -d "llz_version=$LLZ_VERSION" "$ROOT" "$INSTANCE"

# ── 2. Token residue ──────────────────────────────────────────────────────────
step "Token residue (no unrendered copier delimiters)"
# Delimiters from copier.yml _envops. A survivor means a substitution didn't fire.
if grep -RnoE '<@|@>|<%|%>|<#|#>' "$INSTANCE" 2>/dev/null; then
  fail "unrendered copier token(s) found above — a <@ … @> substitution did not resolve"
else
  echo "  none — all copier tokens rendered."
fi

# ── 3. Structure ──────────────────────────────────────────────────────────────
step "Structure (load-bearing files rendered)"
require() { if [[ -e "$INSTANCE/$1" ]]; then echo "  ok   $1"; else fail "missing: $1"; fi; }
require ".copier-answers.yml"
require "renovate.json"
require ".github/workflows/terraform.yml"
require ".github/workflows/cluster-health.yml"
require "apl-values/_shared/values.yaml"
# Operational tooling (the llz CLI, which absorbed instance-scripts/) is
# template-INTERNAL and intentionally NOT delivered: the reusable llz-* workflows
# build it from a template checkout (_llz-template/), so an instance must NOT
# carry its own scripts/ copy. Assert it's absent.
if [[ -d "$INSTANCE/scripts" ]] && [[ -n "$(ls -A "$INSTANCE/scripts" 2>/dev/null)" ]]; then
  fail "scripts/ should not be delivered to instances — operational tooling (llz) is sourced from the template checkout, not copied (a stray copier _task?)"
else
  echo "  ok   no scripts/ delivered (llz tooling sourced from template checkout)"
fi
# _tasks should also deliver the operator-facing docs/ subset (and drop the two
# template-build docs). Spot-check both: a representative operational doc present,
# and the excluded ones absent.
require "docs/runbooks/bootstrap-openbao.md"
require "docs/adopter-guide.md"
absent() { if [[ -e "$INSTANCE/$1" ]]; then fail "should NOT be in instance (template-build doc): $1"; else echo "  ok   absent: $1"; fi; }
absent "docs/templatization-plan.md"
absent "docs/agents.md"

# ── 3b. `llz env add` works INSIDE the rendered instance ──────────────────────
# This is the documented `cd my-instance; llz env add <env>` path. It must work
# with no scripts/ present (the scaffolding is built into the binary). Assert the
# per-env tfvars + overlay land at the instance-root layout terraform.yml expects.
step "llz env add (native scaffolder, inside the rendered instance)"
if [[ -z "${LLZ:-}" || ! -x "$LLZ" ]]; then
  fail "llz binary not available — cannot test env add"
else
  ( cd "$INSTANCE" && git init -q && git add -A && git commit -qm scaffold >/dev/null 2>&1 || true
    "$LLZ" env add itest --region us-ord --obj-cluster us-ord-1 >/dev/null ) \
    || fail "llz env add failed inside the rendered instance"
  for p in \
    "terraform-iac-bootstrap/cluster/itest.tfvars" \
    "terraform-iac-bootstrap/cluster-bootstrap/itest.tfvars" \
    "terraform-iac-bootstrap/object-storage/itest.tfvars" \
    "apl-values/itest/values.yaml"; do
    if [[ -e "$INSTANCE/$p" ]]; then echo "  ok   env add -> $p"; else fail "env add did not create $p"; fi
  done
  # The generated env must carry no surviving 'your-env' value (comments aside).
  if grep -rnH "your-env" "$INSTANCE"/terraform-iac-bootstrap/*/itest.tfvars 2>/dev/null | grep -vE ':[0-9]+:[[:space:]]*#'; then
    fail "env add left an unfilled 'your-env' value above"
  else
    echo "  ok   no leftover 'your-env' value"
  fi
fi

# Instance-local checks: lint configs ship; the checks themselves live in `llz`
# (llz lint/fmt/validate) and the pre-commit hook is installed per-clone by
# `llz hooks`, so neither a Makefile nor a delivered hook is in the scaffold.
require ".tflintrc.hcl"
require ".checkov.yaml"
require ".gitleaks.toml"
absent "Makefile"
absent ".githooks/pre-commit"

# ── 4. Terraform validate (rendered roots, modules rewritten to in-repo) ──────
if [[ "${SKIP_TF:-0}" == "1" ]]; then
  step "Terraform validate (SKIPPED — SKIP_TF=1)"
elif ! command -v "$TF" >/dev/null 2>&1; then
  step "Terraform validate (SKIPPED — '$TF' not found; set TF= or SKIP_TF=1)"
else
  step "Terraform validate ($TF, git:: sources → in-repo terraform-modules/)"
  # Rewrite each published git::…//terraform-modules/<name>?ref=… source to the
  # in-repo module path so init/validate work with no tags, backend, or creds —
  # same approach as template-scripts/ci/instantiate-terraform.sh. The path MUST be a
  # local *relative* source: a relative module source stays in the calling root's
  # Terraform "package", so a module's own internal "../llz-node-firewall" refs
  # can traverse up; an absolute source would make each module its own package and
  # those internal refs would error with "Local module path escapes module
  # package". All roots are siblings at $INSTANCE/terraform-iac-bootstrap/<root>, so one relative
  # path (computed lexically, name is just a depth placeholder) serves them all.
  REL="$(python3 -c 'import os,sys; print(os.path.relpath(sys.argv[1], sys.argv[2]))' \
    "$ROOT/terraform-modules" "$INSTANCE/terraform-iac-bootstrap/_root_")"
  find "$INSTANCE/terraform-iac-bootstrap" -name '*.tf' -print0 | while IFS= read -r -d '' f; do
    REL="$REL" perl -i -pe \
      's{source\s*=\s*"git::[^"]*//terraform-modules/([^"?]+)\?ref=[^"]*"}{source = "$ENV{REL}/$1"}g' \
      "$f"
  done
  for root in "$INSTANCE"/terraform-iac-bootstrap/*/; do
    [[ -f "${root}main.tf" ]] || continue
    echo "  ── ${root#"$INSTANCE"/}"
    if ! ( cd "$root" \
          && "$TF" init -backend=false -input=false -no-color >/dev/null \
          && "$TF" validate -no-color ); then
      fail "terraform validate failed for ${root#"$INSTANCE"/}"
    fi
  done
fi

# ── 5. actionlint (rendered instance workflows) ───────────────────────────────
if command -v actionlint >/dev/null 2>&1; then
  step "actionlint (rendered instance workflows)"
  if ! actionlint "$INSTANCE"/.github/workflows/*.yml; then
    fail "actionlint reported problems in the rendered instance workflows"
  fi
else
  step "actionlint (SKIPPED — not installed)"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
step "Summary"
if [[ "$FAILED" -ne 0 ]]; then
  echo "instance-test: FAILED — see ::error:: lines above. Rendered instance left at ${INSTANCE#"$ROOT"/} for inspection."
  exit 1
fi
echo "instance-test: PASSED — instance rendered + validated at ${INSTANCE#"$ROOT"/} (no cluster stood up)."
