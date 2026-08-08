#!/usr/bin/env bash
# Regenerate the effort/ROI stats for this repo from primary sources.
#
# Scars as defaults:
#   - Logical SLOC, not physical. Physical LOC over-credits a Go+YAML tree by
#     ~35% (blanks + comment blocks) and makes every downstream estimate wrong.
#   - Merge commits are excluded from churn. Including them double-counts every
#     line in a PR-per-merge repo like this one.
#   - Transcript coverage is reported as a FRACTION of commits, never assumed to
#     be 1.0. Local transcripts start whenever this machine started; the repo is
#     older. Extrapolating without printing the fraction silently understates.
#   - Cache reads are broken out. They are ~98% of raw token count and are NOT
#     comparable to output tokens; a single "total tokens" figure is misleading.
set -uo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null)" || {
	echo "not in a git repo" >&2
	exit 1
}

hdr() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

hdr "Delivery"
printf 'commits (no merges)   %s\n' "$(git log --no-merges --oneline | wc -l | tr -d ' ')"
printf 'merge commits (~PRs)  %s\n' "$(git log --merges --oneline | wc -l | tr -d ' ')"
printf 'highest PR number     %s\n' \
	"$(git log --format='%s' --merges | grep -oE 'pull request #[0-9]+' | grep -oE '[0-9]+' | sort -n | tail -1)"
printf 'tracked files         %s\n' "$(git ls-files | wc -l | tr -d ' ')"
first=$(git log --reverse --format='%ad' --date=short | head -1)
last=$(git log -1 --format='%ad' --date=short)
printf 'span                  %s -> %s\n' "$first" "$last"

hdr "Churn (lines ever written, not lines surviving)"
git log --no-merges --numstat --format='' |
	awk '{a+=$1; d+=$2} END {printf "added %d  deleted %d  net %d\n", a, d, a-d}'

hdr "Logical SLOC (blank + comment stripped)"
sloc() { # glob... -> logical line count
	git ls-files "$@" 2>/dev/null | xargs cat 2>/dev/null |
		grep -vE '^[[:space:]]*$' | grep -vcE '^[[:space:]]*(//|#)' | tr -d ' '
}
go_prod=$(git ls-files '*.go' | grep -v '_test.go' | xargs cat 2>/dev/null |
	grep -vE '^[[:space:]]*$' | grep -vcE '^[[:space:]]*//' | tr -d ' ')
go_test=$(sloc '*_test.go')
yaml=$(sloc '*.yaml' '*.yml')
infra=$(sloc '*.tf' '*.sh' '*.tpl' '*.hcl')
docs=$(git ls-files '*.md' | xargs cat 2>/dev/null | grep -vcE '^[[:space:]]*$')
printf 'go production         %s\n' "$go_prod"
printf 'go tests              %s\n' "$go_test"
printf 'yaml / helm / config  %s\n' "$yaml"
printf 'terraform+shell+tpl   %s\n' "$infra"
printf 'CODE TOTAL            %s\n' "$((go_prod + go_test + yaml + infra))"
printf 'markdown (non-blank)  %s   [not SLOC; counted separately]\n' "$docs"
awk -v p="$go_prod" -v t="$go_test" 'BEGIN{printf "test:prod ratio       %.2f:1\n", t/p}'

hdr "Human hours (commit-clustered, 90m idle gap + 30m lead-in)"
git log --no-merges --format='%at' | sort -n | awk '
  NR==1 {s=$1; prev=$1; n=1; next}
  {if ($1-prev > 5400) {tot += prev-s; n++; s=$1} prev=$1}
  END {tot += prev-s; printf "sessions %d   active hours %.1f\n", n, (tot + n*1800)/3600}'
printf 'days with commits     %s\n' "$(git log --no-merges --format='%ad' --date=short | sort -u | wc -l | tr -d ' ')"

hdr "AI tokens + wall-clock (local transcripts)"
tdir="$HOME/.claude/projects/$(pwd | tr '/' '-')"
if [ -d "$tdir" ] && ls "$tdir"/*.jsonl >/dev/null 2>&1; then
	# Deduplication lives in token-usage.py — see the SCAR in its docstring.
	# Do NOT reinline this as `cat *.jsonl | sum`: that counts one API response
	# once per content block and overstates every figure by ~2x.
	python3 "$(dirname "$0")/token-usage.py" "$tdir"/*.jsonl
	# Coverage fraction: transcripts start when THIS machine started, not when the repo did.
	tstart=$(cat "$tdir"/*.jsonl 2>/dev/null | python3 -c '
import sys, json
best = None
for line in sys.stdin:
    try: s = json.loads(line).get("timestamp")
    except Exception: continue
    if s and (best is None or s < best): best = s
print((best or "")[:10])')
	if [ -n "$tstart" ]; then
		inw=$(git log --no-merges --format='%ad' --date=short | awk -v d="$tstart" '$1>=d' | wc -l | tr -d ' ')
		all=$(git log --no-merges --oneline | wc -l | tr -d ' ')
		awk -v i="$inw" -v a="$all" 'BEGIN{printf "commit coverage       %d/%d = %.0f%%  <- divide totals by this to extrapolate\n", i, a, 100*i/a}'
	fi
else
	echo "no local transcripts at $tdir (measured on another machine?)"
fi
echo
