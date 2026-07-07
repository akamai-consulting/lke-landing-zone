#!/usr/bin/env bash
# dispatch-and-watch.sh — trigger a workflow_dispatch in another repo and block
# until the resulting run finishes, propagating its success/failure as the exit
# code. Prints ONLY the run id on stdout (all logs go to stderr) so a caller can
# capture it with RUN_ID="$(dispatch-and-watch.sh ...)".
#
# Requires: gh (authenticated via GH_TOKEN) with actions:read/write on --repo.
#
# Usage:
#   dispatch-and-watch.sh --repo OWNER/REPO --workflow FILE.yml \
#     --field key=value [--field key=value ...] [--ref BRANCH] [--timeout SECS]

set -euo pipefail

log() { printf '%s\n' "$*" >&2; }

REPO=""; WORKFLOW=""; REF="main"; TIMEOUT=10800   # 3h default cap
FIELDS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)     REPO="$2"; shift 2 ;;
    --workflow) WORKFLOW="$2"; shift 2 ;;
    --ref)      REF="$2"; shift 2 ;;
    --timeout)  TIMEOUT="$2"; shift 2 ;;
    --field)    FIELDS+=("--field" "$2"); shift 2 ;;
    *) log "unknown arg: $1"; exit 2 ;;
  esac
done
[[ -n "$REPO" && -n "$WORKFLOW" ]] || { log "usage: --repo and --workflow are required"; exit 2; }
command -v gh >/dev/null 2>&1 || { log "gh CLI not found"; exit 2; }

# Marker so we can pick OUR run out of the list: record the UTC second just before
# dispatch and match the newest workflow_dispatch run created at/after it.
BEFORE_EPOCH="$(date -u +%s)"

log "dispatch: ${REPO} ${WORKFLOW} (ref=${REF}) ${FIELDS[*]}"
# A freshly force-pushed workflow can take a few seconds to become dispatchable.
dispatched=0
for attempt in 1 2 3 4 5 6; do
  if gh workflow run "$WORKFLOW" --repo "$REPO" --ref "$REF" "${FIELDS[@]}" >&2; then
    dispatched=1; break
  fi
  log "dispatch attempt ${attempt} failed (workflow may still be indexing) — retrying in 10s"
  sleep 10
done
[[ "$dispatched" -eq 1 ]] || { log "could not dispatch ${WORKFLOW} on ${REPO}"; exit 1; }

# Find the run id (gh workflow run does not return it). Poll for up to 90s.
RUN_ID=""
for _ in $(seq 1 30); do
  sleep 3
  # Newest dispatch run; compare its createdAt to our pre-dispatch timestamp.
  read -r RID CREATED < <(gh run list --repo "$REPO" --workflow "$WORKFLOW" \
      --event workflow_dispatch --limit 1 \
      --json databaseId,createdAt --jq '.[0] | "\(.databaseId) \(.createdAt)"' 2>/dev/null || echo "")
  [[ -n "${RID:-}" ]] || continue
  CREATED_EPOCH="$(date -u -d "$CREATED" +%s 2>/dev/null || date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "$CREATED" +%s 2>/dev/null || echo 0)"
  if [[ "$CREATED_EPOCH" -ge "$((BEFORE_EPOCH - 5))" ]]; then
    RUN_ID="$RID"; break
  fi
done
[[ -n "$RUN_ID" ]] || { log "could not locate the dispatched run within 90s"; exit 1; }
log "watching run ${RUN_ID}: $(gh run view "$RUN_ID" --repo "$REPO" --json url --jq .url 2>/dev/null || echo "")"

# Block until the run is genuinely COMPLETED. `gh run watch --exit-status` can exit
# non-zero WITHOUT the run having finished — most commonly an HTTP 404 on the run's
# jobs endpoint in the seconds after dispatch, before GitHub lists the jobs (observed:
# watch attached ~4s post-create, 404'd, exited rc=1 while the destroy ran on fine —
# failing teardown on a healthy run). A single watch exit is therefore NOT proof the
# run ended, so re-attach until the run's status is actually 'completed'. Bound the
# whole wait by TIMEOUT so a hung run still can't pin this job.
deadline=$(( $(date -u +%s) + TIMEOUT ))
watch_rc=0
run_status=""
while :; do
  remaining=$(( deadline - $(date -u +%s) ))
  if [[ $remaining -le 0 ]]; then
    log "::error::run ${RUN_ID} exceeded ${TIMEOUT}s — cancelling"
    gh run cancel "$RUN_ID" --repo "$REPO" >&2 || true
    exit 124
  fi
  set +e
  timeout "$remaining" gh run watch "$RUN_ID" --repo "$REPO" --interval 30 --exit-status >&2
  watch_rc=$?
  set -e
  if [[ $watch_rc -eq 124 ]]; then
    log "::error::run ${RUN_ID} exceeded ${TIMEOUT}s — cancelling"
    gh run cancel "$RUN_ID" --repo "$REPO" >&2 || true
    exit 124
  fi
  run_status="$(gh run view "$RUN_ID" --repo "$REPO" --json status --jq '.status // ""' 2>/dev/null || echo "")"
  [[ "$run_status" == "completed" ]] && break
  # Watch returned but the run is still going: a transient API hiccup (e.g. the
  # jobs-endpoint 404 right after dispatch) detached it early. Wait a beat and
  # re-attach rather than mistaking it for a run failure.
  log "watch detached before completion (rc=${watch_rc}, run status='${run_status:-unknown}') — re-attaching in 10s"
  sleep 10
done

# Even once the run is completed, do NOT trust the watch exit code alone: it has been
# observed to return 0 for a run that had ALREADY finished (e.g. failed within seconds,
# before the watch attached) — which silently turned a failing dispatched run into a
# green caller job. Authoritatively re-read the run's conclusion and require success.
# (A run whose jobs were all skipped concludes 'success' too; that's intended —
# skip-only no-ops are prevented upstream by forwarding the dispatch inputs.)
CONCLUSION="$(gh run view "$RUN_ID" --repo "$REPO" --json conclusion --jq '.conclusion // "unknown"' 2>/dev/null || echo unknown)"
if [[ "$CONCLUSION" != "success" ]]; then
  url="$(gh run view "$RUN_ID" --repo "$REPO" --json url --jq .url 2>/dev/null || echo "")"
  log "::error::run ${RUN_ID} concluded '${CONCLUSION}' (watch rc=${watch_rc}) — ${url}"
  exit 1
fi

# Success: emit the run id on stdout for the caller to capture.
printf '%s\n' "$RUN_ID"
