#!/usr/bin/env bash
# install-llz.sh — download the `llz` CLI from a template release via `gh` and
# install it to ~/.local/bin. It resolves the release the same way the CLI does,
# picks your platform's asset, verifies the checksum, drops the binary on a
# sudo-free corp-friendly path, and then reports which `llz` your shell will
# actually run — installing one and running another is the failure this script
# exists to prevent.
#
#   ./template-scripts/install-llz.sh                 # latest full release
#   ./template-scripts/install-llz.sh v0.0.39         # a specific tag
#   ORG=myfork ./template-scripts/install-llz.sh      # install from your fork's releases
#   LLZ_BINDIR=/usr/local/bin ./template-scripts/install-llz.sh   # custom install dir
#
# Requires `gh`, authenticated (`gh auth status`) — run `gh auth login` first. The
# template repo is public, but the script drives `gh` so it also works against a
# private fork or a GHE host (gh inherits your auth).
set -euo pipefail

ORG="${ORG:-akamai-consulting}"
REPO="${ORG}/lke-landing-zone"
BINDIR="${LLZ_BINDIR:-$HOME/.local/bin}"
VER="${1:-${LLZ_VERSION:-}}"
# Host the release lives on — github.com unless GH_HOST points gh at a GHE fork.
HOST="${GH_HOST:-github.com}"

command -v gh >/dev/null || {
  echo "install-llz: gh not found — install the GitHub CLI first (it authenticates the private-repo download)." >&2
  exit 1
}
# Scope the auth check to the one host we download from. Bare `gh auth status`
# exits non-zero if ANY configured host is broken (e.g. an expired token on an
# unrelated GHE account), which would wrongly block a user logged in to $HOST.
gh auth status --hostname "$HOST" >/dev/null 2>&1 || {
  echo "install-llz: gh is not authenticated to $HOST — run \`gh auth login --hostname $HOST\` first." >&2
  echo "install-llz: (any other gh hosts can be in any state; only $HOST matters here.)" >&2
  exit 1
}

# Resolve the latest full release when no version is given — the SAME rule
# `llz self-update` / `llz new` apply (llzver.LatestLLZTag, tools/internal/shared/llzver/llzver.go),
# because the two must agree: the installer picks the binary, and that binary's
# own version is what `llz new` pins the instance to. Highest SEMVER, not newest
# by date — `--limit 1` returned whatever was released last, so a patch
# back-ported to an older line would install vX while the CLI pinned vY.
#
#   isDraft/isPrerelease  dropped. A pre-release is an unpromoted e2e candidate
#                         (RELEASING.md). A draft is worse than useless: it is
#                         visible only to users with push access, and its git tag
#                         does NOT exist yet — so the download succeeds and the
#                         scaffold then dies in copier with "pathspec 'vX.Y.Z'
#                         did not match any file(s) known to git".
#   the tag regex        drops prefixed tags (the legacy llz/v* CLI track, any
#                        other release track) and malformed ones — the repo
#                        carries a real `v.0.0.30` typo tag that must not win.
#                        A -pre/+build tail is tolerated and then stripped for
#                        ordering, exactly as semver() does, so the two agree on
#                        a tag like v1.2.3-hotfix that is not flagged pre-release.
#
# The reduce is not a stylistic choice over `sort_by(.key) | last`. Stripping the
# tail means two full releases can share one numeric core (v1.2.3 and
# v1.2.3-hotfix), and jq's sort is stable, so `last` returns the LAST of equals —
# which is the OLDEST, since gh lists newest first. LatestLLZTag replaces its
# best only on a strictly greater version, keeping the first (newest) of equals;
# `$r.key > .key` reproduces exactly that. Pinned by
# TestLatestLLZTagTieKeepsFirst.
# shellcheck disable=SC2016  # $r is a jq variable; it must NOT expand in the shell.
tag_query='[.[] | select((.isDraft or .isPrerelease) | not) | .tagName
     | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+([-+].*)?$"))]
   | map({tag: ., key: (sub("^v";"") | sub("[-+].*$";"") | split(".") | map(tonumber))})
   | reduce .[] as $r (null; if . == null or $r.key > .key then $r else . end)
   | (.tag // empty)'

if [ -z "$VER" ]; then
  VER="$(gh release list --repo "$REPO" --limit 200 --json tagName,isDraft,isPrerelease --jq "$tag_query")"
  [ -n "$VER" ] || {
    echo "install-llz: no full vX.Y.Z release found in $REPO — pass a version, e.g. \`$0 v0.0.39\`." >&2
    exit 1
  }
  echo "install-llz: latest release is $VER"
else
  # An explicitly requested tag skips the filter above, so check the one property
  # that silently breaks `llz new` later: a draft release has no git tag.
  if [ "$(gh release view "$VER" --repo "$REPO" --json isDraft --jq '.isDraft' 2>/dev/null)" = "true" ]; then
    echo "install-llz: $VER is a DRAFT release in $REPO — its git tag does not exist, so the binary would" >&2
    echo "install-llz: install but \`llz new\` would fail (\"pathspec '$VER' did not match any file(s) known" >&2
    echo "install-llz: to git\"). Publish the release first, or install a published tag." >&2
    exit 1
  fi
fi

# Map uname → release asset suffix (llz-<os>-<arch>).
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "install-llz: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) echo "install-llz: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
asset="llz-${os}-${arch}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$BINDIR"

# gh's own failure here is just "release not found" — no tag, no repo, no hint —
# and this is the line a mistyped or unpublished explicit tag lands on, so say
# which release was looked for and where to see the real list.
gh release download "$VER" --repo "$REPO" \
  --pattern "$asset" --pattern SHA256SUMS --clobber --dir "$tmp" || {
  echo "install-llz: could not download $asset from $REPO at $VER." >&2
  echo "install-llz: check the tag exists and has assets:  gh release list --repo $REPO" >&2
  exit 1
}

# Verify the checksum (sha256sum on Linux, shasum on macOS).
if command -v sha256sum >/dev/null; then sum="sha256sum"; else sum="shasum -a 256"; fi
(cd "$tmp" && grep " ${asset}\$" SHA256SUMS | $sum -c -)

install -m 0755 "$tmp/$asset" "$BINDIR/llz"
# `</dev/null` for the same reason as the probes below: under `curl … | bash`,
# stdin IS the installer script, and any child that reads it truncates the rest.
echo "install-llz: installed $("$BINDIR/llz" version </dev/null) → $BINDIR/llz"
echo "install-llz: enable shell completion with \`llz completion zsh\` (or \`bash\`) — see quickstart §2."

# ── which llz will the shell actually run? ───────────────────────────────────
# Installing the binary is not the same as the shell RUNNING it. Another llz
# earlier on PATH silently wins every lookup, and this script's own success line
# above is no evidence otherwise — it invokes $BINDIR/llz by absolute path, so it
# reports the version nobody is going to execute. Real sources of a competing
# copy: a quickstart older than 2026-06-13 installed with `sudo` into
# /usr/local/bin, the devcontainer image ships one there, `make llz` leaves one
# in a checkout's bin/, and `go install` drops one in ~/go/bin. A stale winner
# fails LATER and cryptically — an old binary scaffolds from a template ref that
# no longer exists ("pathspec 'vX.Y.Z' did not match any file(s) known to git")
# and self-updates against a retired tag track ("release not found"). So
# enumerate every copy, name the one that wins, and say how to fix it.

# llz_version_of prints a binary's version line, or a placeholder if it won't run.
#
# `</dev/null` is load-bearing, not hygiene. The documented install path is
# `curl … | bash`, where bash reads THE SCRIPT ITSELF from stdin — so any child
# process that reads stdin eats the rest of the installer, and bash then just
# stops wherever the buffer ran out. These probes execute whatever files happen
# to be named `llz` on this machine, which is precisely the code we cannot vouch
# for. Detach them from stdin so a stray `llz` that reads it truncates nothing.
# `|| true` inside the braces, because `set -o pipefail` is on: when the probed
# binary prints more than `head` reads, it takes SIGPIPE (141) once head exits,
# pipefail promotes that to the pipeline's status, and the captured line is
# thrown away — reporting "(not runnable)" for a binary that answered correctly.
# Measured with a stub printing 200k lines: empty before, right version after.
# Swallowing the status here is safe because emptiness is the real signal.
#
# Only TWO binaries are ever probed: the one just installed (ours, checksum-
# verified) and the one the shell resolves (which `llz` runs regardless of this
# script). Every other copy is reported by path alone — see report_other.
llz_version_of() {
  v="$({ "$1" version </dev/null 2>/dev/null || true; } | head -n 1)"
  [ -n "$v" ] || v="$({ "$1" --version </dev/null 2>/dev/null || true; } | head -n 1)"
  [ -n "$v" ] || v="(not runnable)"
  printf '%s' "$v"
}

# llz_on_path lists every executable `llz` reachable via PATH, in PATH order,
# deduped (a PATH that repeats a dir — e.g. the export line below pasted twice —
# would otherwise report the same binary two or three times).
llz_on_path() {
  printf '%s\n' "$PATH" | tr ':' '\n' | while IFS= read -r d; do
    [ -n "$d" ] || continue
    if [ -f "$d/llz" ] && [ -x "$d/llz" ]; then printf '%s\n' "$d/llz"; fi
  done | awk '!seen[$0]++'
}

# `type -P` searches PATH and prints a FILE PATH, which is the only thing the -ef
# comparisons and the `rm` suggestion below can act on. `command -v` would answer
# about the shell's whole namespace: with an exported `llz` function in the
# environment (`export -f llz` in the calling shell — inherited even by a
# non-interactive script) it prints the bare word "llz", which then reads as a
# relative path and turns the report into nonsense (`rm llz`). Aliases can't reach
# here at all — a non-interactive bash script inherits none.
winner="$(type -P llz 2>/dev/null || true)"

if [ -z "$winner" ]; then
  echo "install-llz: NOTE $BINDIR is not on your PATH — \`llz\` won't resolve yet." >&2
  echo "install-llz:   echo 'export PATH=\"$BINDIR:\$PATH\"' >> ~/.zshrc   # then restart the shell" >&2
elif [ "$winner" -ef "$BINDIR/llz" ]; then
  echo "install-llz: \`llz\` on your PATH → $winner ($(llz_version_of "$winner"))"
else
  {
    echo
    echo "install-llz: ⚠  ANOTHER llz WINS ON YOUR PATH — the one just installed is shadowed."
    echo "install-llz:    in use →  $winner  [$(llz_version_of "$winner")]"
    echo "install-llz:    new    →  $BINDIR/llz  [$(llz_version_of "$BINDIR/llz")]"
    echo "install-llz:    \`llz version\` will keep reporting the OLD binary, and a stale llz can fail"
    echo "install-llz:    later with e.g. \"pathspec 'vX.Y.Z' did not match any file(s) known to git\"."
    echo "install-llz:    Fix it one of two ways, then re-check with \`hash -r; type -a llz\`:"
    echo "install-llz:      1) drop the old copy (prefix with sudo if it is root-owned):"
    echo "install-llz:           rm '$winner'"
    echo "install-llz:      2) or put the new one first, then rehash the shell (zsh: rehash):"
    echo "install-llz:           export PATH=\"$BINDIR:\$PATH\" && hash -r"
  } >&2
fi

# Any remaining copy is inert today but becomes the next shadow the moment PATH
# changes (a second shell's rc file, a devcontainer, a `sudo` install), so list
# them once with their versions instead of leaving them to be discovered by a
# confusing failure months later. Off-PATH dirs are worth naming for the same
# reason: they are exactly what a later PATH edit promotes to winner.
#
# These are listed by PATH ONLY — deliberately not executed. Probing a version
# means running the file, and this loop reaches copies the user has not chosen to
# run: an `llz` in a dir that is not even on PATH would otherwise sit dormant,
# and executing it here is the installer's decision, not theirs. The path is the
# actionable part ("this one may shadow you later"); anyone who wants the version
# can run it themselves. The two binaries that ARE probed above are the two the
# user is already committed to: ours, and whatever `llz` already resolves to.
report_other() { # $1 = path, $2 = suffix note
  if [ "$others_header" = "0" ]; then
    echo "install-llz: other llz copies on this machine (not run, not in use right now):" >&2
    others_header=1
  fi
  echo "install-llz:    $1$2" >&2
}
others_header=0
while IFS= read -r p; do
  [ -n "$p" ] || continue
  [ ! "$p" -ef "$BINDIR/llz" ] || continue
  { [ -z "$winner" ] || [ ! "$p" -ef "$winner" ]; } || continue
  report_other "$p" ""
done <<EOF
$(llz_on_path)
EOF
for d in /usr/local/bin /opt/homebrew/bin /opt/local/bin /usr/bin "$HOME/.local/bin" "$HOME/go/bin" "$HOME/bin"; do
  # The PATH test is textual, so a dir PATH spells differently (a trailing slash,
  # a symlink) reads as absent here. Re-check by inode against the two files we
  # have already accounted for — otherwise the winner itself can be re-listed
  # below as "not in use right now", which is the one thing this block must never
  # say about the binary the shell is running.
  case ":$PATH:" in *":$d:"*) continue ;; esac
  if [ -f "$d/llz" ] && [ -x "$d/llz" ] && [ ! "$d/llz" -ef "$BINDIR/llz" ] &&
    { [ -z "$winner" ] || [ ! "$d/llz" -ef "$winner" ]; }; then
    report_other "$d/llz" "  (not on PATH)"
  fi
done
