#!/usr/bin/env bash
# stamp-template-version.sh — record the LLZ template provenance of this repo.
#
# Writes a committed `.template-version` (JSON) at the repo root capturing which
# template repo + ref + commit the instance was generated/last synced from. This
# is the provenance `llz drift` (and the Scheduled Checks template-drift job,
# which runs it) reads to report how far behind an instance has fallen.
#
# `llz env add` stamps natively on scaffold; re-run it by hand after you sync
# upstream template changes so the stamp tracks the new baseline.
#
# Usage:
#   template-scripts/stamp-template-version.sh [options]
#
# Options:
#   --repo OWNER/REPO|URL   Template repo (default: the `upstream` remote, else
#                           `origin`, else TEMPLATE_REPO env, else akamai-consulting/lke-landing-zone)
#   --ref REF               Template ref (default: git describe --tags --always)
#   --sha SHA               Template commit (default: git rev-parse HEAD)
#   --env NAME              Record the env this stamp was last generated for (informational)
#   --now TS                Override the timestamp (default: date -u, ISO8601). For reproducible runs.
#   -h, --help              Show this help
#
# Exit codes: 0 written; 1 usage/validation error

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=template-scripts/lib-common.sh
source "$SCRIPT_DIR/lib-common.sh"

DEFAULT_REPO="akamai-consulting/lke-landing-zone"
REPO=""
REF=""
SHA=""
ENV_NAME=""
NOW=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --ref)  REF="$2"; shift 2 ;;
    --sha)  SHA="$2"; shift 2 ;;
    --env)  ENV_NAME="$2"; shift 2 ;;
    --now)  NOW="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
done

git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "not inside a git work tree"
REPO_ROOT="$(git rev-parse --show-toplevel)"

# Template repo: explicit flag → env → `upstream` remote → `origin` → default.
if [[ -z "$REPO" ]]; then
  REPO="${TEMPLATE_REPO:-}"
fi
if [[ -z "$REPO" ]]; then
  REPO="$(git remote get-url upstream 2>/dev/null || git remote get-url origin 2>/dev/null || true)"
fi
[[ -n "$REPO" ]] || REPO="$DEFAULT_REPO"

: "${SHA:=$(git rev-parse HEAD)}"
: "${REF:=$(git describe --tags --always 2>/dev/null || git rev-parse --abbrev-ref HEAD)}"
: "${NOW:=$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

OUT="$REPO_ROOT/.template-version"

# Preserve the first-seen env if a stamp already exists and no --env was passed.
if [[ -z "$ENV_NAME" && -f "$OUT" ]] && command -v jq >/dev/null 2>&1; then
  ENV_NAME="$(jq -r '.env // ""' "$OUT" 2>/dev/null || echo "")"
fi

# Emit JSON without depending on jq (so the stamp works on a minimal runner).
json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
cat > "$OUT" <<EOF
{
  "schema": 1,
  "template_repo": "$(json_escape "$REPO")",
  "template_ref": "$(json_escape "$REF")",
  "template_sha": "$(json_escape "$SHA")",
  "generator": "new-deployment.sh",
  "stamped_at": "$(json_escape "$NOW")",
  "env": "$(json_escape "$ENV_NAME")"
}
EOF

echo "Stamped ${OUT#"$REPO_ROOT"/}: ${REPO} @ ${REF} (${SHA:0:8})"
