#!/usr/bin/env bash
#
# Delete all GitHub Actions run history for workflows that were renamed or merged.
# Once all runs are deleted the workflow disappears from the Actions sidebar.
#
# Prerequisites:
#   - gh CLI installed and authenticated (gh auth login)
#   - Run from anywhere inside the repo (uses git remote to detect REPO)
#
# Usage:
#   ./template-scripts/linting-and-validation/cleanup-workflow-runs.sh [--dry-run]
#
# Options:
#   --dry-run   Print what would be deleted without deleting anything.

set -euo pipefail

DRY_RUN=false
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=true
  echo "DRY RUN — no runs will be deleted."
fi

REPO=$(gh repo view --json nameWithOwner --jq '.nameWithOwner')
echo "Repo: $REPO"

# Workflows that were renamed or merged and should be cleaned from the sidebar.
WORKFLOWS=(
  ci.yml
  fuzz.yml
  scorecard.yml
  trivy.yml
  build-firewall-controller.yml
  sync-firewall-cidrs.yml
  firewall.yml
)

delete_runs() {
  local workflow="$1"
  echo
  echo "── $workflow ──────────────────────────────────────────"

  local ids
  ids=$(gh run list \
    --repo "$REPO" \
    --workflow "$workflow" \
    --limit 1000 \
    --json databaseId \
    --jq '.[].databaseId' 2>/dev/null || true)

  if [[ -z "$ids" ]]; then
    echo "  No runs found — already clean."
    return
  fi

  local count
  count=$(echo "$ids" | wc -l | tr -d ' ')
  echo "  Found $count run(s)."

  if $DRY_RUN; then
    echo "  [dry-run] Would delete: $(echo "$ids" | tr '\n' ' ')"
    return
  fi

  local deleted=0
  while IFS= read -r id; do
    if gh run delete "$id" --repo "$REPO" 2>/dev/null; then
      (( deleted++ )) || true
    else
      echo "  Warning: could not delete run $id (may already be gone)"
    fi
  done <<< "$ids"

  echo "  Deleted $deleted run(s)."
}

for wf in "${WORKFLOWS[@]}"; do
  delete_runs "$wf"
done

echo
echo "Done. Workflows with no remaining runs will disappear from the sidebar."
