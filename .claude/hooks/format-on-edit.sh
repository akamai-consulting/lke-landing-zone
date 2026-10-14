#!/usr/bin/env bash
# PostToolUse (Write|Edit): auto-format the file that was just edited so the
# fmt gates (`make fmt-check`, `make tf-fmt-check`) cannot fail later. Mirrors
# `make fmt` (gofmt -w in tools/) and `make tf-fmt` (tofu fmt on the reusable
# modules). Always exits 0 — formatting is best-effort, the make gates stay
# authoritative.
set -uo pipefail

command -v jq >/dev/null 2>&1 || exit 0
file=$(jq -r '.tool_input.file_path // empty' 2>/dev/null || true)
[ -n "$file" ] && [ -f "$file" ] || exit 0

case "$file" in
  */tools/*.go)
    if command -v gofmt >/dev/null 2>&1; then
      gofmt -w "$file" || true
    fi
    ;;
  */terraform-modules/*.tf)
    # Only the reusable modules: instance-template/ .tf files can carry copier
    # <@ @> tokens that are not valid HCL, so they are never auto-formatted.
    if command -v tofu >/dev/null 2>&1; then
      tofu fmt "$file" >/dev/null 2>&1 || true
    fi
    ;;
esac
exit 0
