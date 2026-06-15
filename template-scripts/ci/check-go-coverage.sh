#!/usr/bin/env bash
# Enforce PER-PACKAGE minimum statement coverage from a Go coverprofile.
#
# Usage: check-go-coverage.sh <coverprofile> <pkg-suffix=min>...
#
# Each threshold's pkg-suffix is matched against the END of a package's Go
# import path, so "cmd/llz" matches .../tools/cmd/llz. The build fails if any
# listed package's coverage is below its floor, or if a listed package produced
# no coverage data at all (a missing/renamed package shouldn't pass silently).
# Packages without a threshold are not gated.
set -euo pipefail

profile="${1:?usage: check-go-coverage.sh <coverprofile> <pkg-suffix=min>...}"
shift

[[ -f "$profile" ]] || { echo "coverage: profile not found: $profile" >&2; exit 1; }

# Per-package coverage from the profile: covered statements / total statements.
# Profile data lines look like:
#   <import-path>/<file>.go:<a>.<b>,<c>.<d> <numStmt> <hitCount>
# Derive the package import path by stripping the trailing /<file>.go.
pkg_coverage="$(awk '
  NR == 1 { next }                          # skip the "mode:" header line
  {
    split($1, seg, ":"); file = seg[1]
    dir = file; sub(/\/[^\/]*$/, "", dir)
    total[dir] += $2
    if ($3 + 0 > 0) covered[dir] += $2
  }
  END {
    for (d in total)
      printf "%s %.1f\n", d, (total[d] ? covered[d] / total[d] * 100 : 0)
  }
' "$profile")"

fail=0
for entry in "$@"; do
  pkg="${entry%=*}"; min="${entry#*=}"
  # The package whose import path ends in /<pkg>.
  line="$(awk -v p="/$pkg" '$1 ~ (p "$") { print; exit }' <<<"$pkg_coverage")"
  if [[ -z "$line" ]]; then
    printf '  ??   %-26s no coverage data (min %s%%)\n' "$pkg" "$min"
    fail=1; continue
  fi
  pct="${line##* }"
  if awk -v a="$pct" -v b="$min" 'BEGIN { exit (a + 0 < b + 0) ? 0 : 1 }'; then
    printf '  FAIL %-26s %5s%% (min %s%%)\n' "$pkg" "$pct" "$min"
    fail=1
  else
    printf '  ok   %-26s %5s%% (min %s%%)\n' "$pkg" "$pct" "$min"
  fi
done

if (( fail )); then
  echo "coverage: one or more packages are below their per-package threshold." >&2
  echo "          Add tests to raise them, or adjust COVERAGE_MINS in the Makefile." >&2
  exit 1
fi
echo "coverage: all gated packages meet their per-package thresholds."
