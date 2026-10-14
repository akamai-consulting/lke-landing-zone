#!/usr/bin/env bash
# with-retry.sh CMD [ARGS...] — run CMD with bounded retry + linear backoff on a
# transient failure, for flaky network fetches in CI (helm repo-index refreshes and
# chart-tarball pulls, tool downloads, …) where a one-off upstream 5xx / DNS / TLS
# blip should not fail an otherwise-green build.
#
# Pipe-SAFE as a pipeline SOURCE: it buffers the command's stdout and prints it only
# on the SUCCESSFUL attempt, so a failed attempt's partial output never reaches the
# downstream pipe. That is what makes `with-retry.sh helm template … | yq | kubectl`
# correct — a naive `for i in 1 2 3; do helm template …; done | yq` would stream a
# half-rendered manifest from a failed attempt into yq.
#
# It does NOT retry on a clean non-zero that isn't transient in the usual sense — it
# retries ANY non-zero (the caller picks commands where a retry is safe, i.e.
# idempotent reads/renders). Do not wrap stateful mutations (e.g. `helm install`)
# whose partial effect makes a retry conflict.
#
# Tunables (env): WITH_RETRY_ATTEMPTS (default 3), WITH_RETRY_DELAY seconds (default 3;
# backoff is attempt*DELAY).
set -uo pipefail

attempts="${WITH_RETRY_ATTEMPTS:-3}"
delay="${WITH_RETRY_DELAY:-3}"
[ "$#" -ge 1 ] || { echo "with-retry: a command is required" >&2; exit 2; }

err="$(mktemp)"
trap 'rm -f "$err"' EXIT

n=0
while :; do
  # Capture rc from the command itself. Do NOT wrap the run in `if …; then`: an if
  # whose condition fails and has no branch returns 0, so `rc=$?` after it would read
  # 0 and this helper would silently swallow a permanent failure.
  out="$("$@" 2>"$err")" && { printf '%s\n' "$out"; exit 0; }
  rc=$?
  n=$((n + 1))
  if [ "$n" -ge "$attempts" ]; then
    cat "$err" >&2
    echo "with-retry: '$*' failed after ${attempts} attempt(s) (rc=${rc})" >&2
    exit "$rc"
  fi
  echo "with-retry: '$*' failed (rc=${rc}) — retrying $((n + 1))/${attempts} in $((n * delay))s" >&2
  sleep "$((n * delay))"
done
