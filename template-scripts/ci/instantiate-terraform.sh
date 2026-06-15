#!/usr/bin/env bash
# instantiate-terraform.sh — Materialize the instance Terraform roots so the
# template repo can validate them without published module tags or remote state.
#
# The instance roots (instance-template/terraform-iac-bootstrap/<root>) reference the reusable
# modules by their PUBLISHED git:: ref (e.g.
#   source = "git::ssh://…//terraform-modules/llz-cluster?ref=llz-cluster/v0.1.0")
# which only resolves after a release tag exists. This is the Terraform analog of
# rendering charts: it copies the roots into a build dir and rewrites those
# git:: sources to the in-repo terraform-modules/ path, then runs
# `terraform init -backend=false` + `terraform validate` on each — catching HCL,
# type, and module-wiring errors with no tags, no backend, and no credentials.
#
# Usage: template-scripts/ci/instantiate-terraform.sh
# Env:   TF_INSTANCE_DIR  build dir (default: .tf-instance)
#        TF               terraform binary (default: terraform, falling back to tofu)
set -euo pipefail

# shellcheck source=template-scripts/lib-common.sh
source "$(dirname "$0")/../lib-common.sh"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUILD="${TF_INSTANCE_DIR:-.tf-instance}"

# This root MUST run a binary (it validates the instance), so a missing
# terraform/tofu is fatal here rather than a skip.
TF="$(detect_tf)"
[[ -n "$TF" ]] || { echo "::error::neither terraform nor tofu found on PATH; set TF= to your binary" >&2; exit 1; }

cd "$ROOT"
rm -rf "$BUILD"
mkdir -p "$BUILD"

# Mirror instance-template/terraform-iac-bootstrap/ at the SAME depth (<build>/terraform-iac-bootstrap/<root>)
# so the rewritten ../../../terraform-modules path resolves to the repo-root
# modules from each root.
cp -R instance-template/terraform-iac-bootstrap "$BUILD/terraform-iac-bootstrap"
REL="../../../terraform-modules"

find "$BUILD/terraform-iac-bootstrap" -name '*.tf' -print0 | while IFS= read -r -d '' f; do
  perl -i -pe \
    's{source\s*=\s*"git::[^"]*//terraform-modules/([^"?]+)\?ref=[^"]*"}{source = "'"$REL"'/$1"}g' \
    "$f"
done

rc=0
for root in "$BUILD"/terraform-iac-bootstrap/*/; do
  [ -f "${root}main.tf" ] || continue
  echo "── validating ${root#"$BUILD"/}"
  if ! ( cd "$root" \
        && "$TF" init -backend=false -input=false -no-color >/dev/null \
        && "$TF" validate -no-color ); then
    echo "::error::terraform validate failed for ${root#"$BUILD"/}"
    rc=1
  fi
done
exit "$rc"
