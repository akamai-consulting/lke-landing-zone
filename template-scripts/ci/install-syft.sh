#!/usr/bin/env bash
# Install syft for `make sbom-terraform`.
#
# Kept alongside trivy because trivy does not parse `.terraform.lock.hcl` for
# provider inventory — syft does. Trivy handles everything else (k8s image
# SBOMs, the CVE gate). See template-scripts/ci/install-trivy.sh.
#
# macOS + brew: `brew install syft` (matches the repo's install-tools convention).
# Anywhere else: SHA-verified tarball from Anchore's GitHub release, into
# $SYFT_INSTALL_DIR (default $HOME/.local/bin), no sudo.
#
# Override:
#   SYFT_VERSION       — pinned release (default below; bump deliberately)
#   SYFT_INSTALL_DIR   — where to drop the binary
set -euo pipefail

# shellcheck source=template-scripts/lib-common.sh
source "$(dirname "$0")/../lib-common.sh"

SYFT_VERSION="${SYFT_VERSION:-1.18.1}"
SYFT_INSTALL_DIR="${SYFT_INSTALL_DIR:-$HOME/.local/bin}"

if command -v syft >/dev/null 2>&1; then
    echo "syft already installed: $(syft version 2>/dev/null | awk '/Version/ {print $2; exit}' || echo unknown)"
    exit 0
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "Unsupported arch: $arch"; exit 1 ;;
esac

if [ "$os" = "darwin" ] && command -v brew >/dev/null 2>&1; then
    echo "Installing syft via brew."
    brew install syft
    exit 0
fi

tarball="syft_${SYFT_VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}"

install_release_tarball syft "$SYFT_INSTALL_DIR" \
    "${base}/${tarball}" "${base}/syft_${SYFT_VERSION}_checksums.txt" "$tarball"
