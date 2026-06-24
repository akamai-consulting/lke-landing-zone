#!/usr/bin/env bash
# install-llz.sh — download the `llz` CLI from a template release via `gh` and
# install it to ~/.local/bin. Pinned-release installer (no decision logic): it
# resolves your platform's asset, verifies the checksum, and drops the binary on
# a sudo-free, corp-friendly path.
#
#   ./template-scripts/install-llz.sh                 # latest full release
#   ./template-scripts/install-llz.sh v0.2.0          # a specific tag
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

# Resolve the latest full (non-prerelease) tag when no version is given.
if [ -z "$VER" ]; then
  VER="$(gh release list --repo "$REPO" --exclude-pre-releases --limit 1 --json tagName --jq '.[0].tagName')"
  [ -n "$VER" ] || {
    echo "install-llz: could not find a release in $REPO — pass a version, e.g. \`$0 v0.2.0\`." >&2
    exit 1
  }
  echo "install-llz: latest release is $VER"
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

gh release download "$VER" --repo "$REPO" \
  --pattern "$asset" --pattern SHA256SUMS --clobber --dir "$tmp"

# Verify the checksum (sha256sum on Linux, shasum on macOS).
if command -v sha256sum >/dev/null; then sum="sha256sum"; else sum="shasum -a 256"; fi
(cd "$tmp" && grep " ${asset}\$" SHA256SUMS | $sum -c -)

install -m 0755 "$tmp/$asset" "$BINDIR/llz"
echo "install-llz: installed $("$BINDIR/llz" version) → $BINDIR/llz"

case ":$PATH:" in
  *":$BINDIR:"*) ;;
  *) echo "install-llz: add $BINDIR to your PATH:  echo 'export PATH=\"$BINDIR:\$PATH\"' >> ~/.zshrc" >&2 ;;
esac
echo "install-llz: enable shell completion with \`llz completion zsh|bash\` (see quickstart §2)."
