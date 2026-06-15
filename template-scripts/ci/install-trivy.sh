#!/usr/bin/env bash
# Install trivy for `make sbom-terraform` / `make sbom-kubernetes` / `make sbom-scan`.
#
# macOS + brew: `brew install trivy` (matches the repo's install-tools convention).
# Anywhere else: SHA-verified tarball from Aqua's GitHub release, into
# $TRIVY_INSTALL_DIR (default $HOME/.local/bin), no sudo.
#
# Override:
#   TRIVY_VERSION       — pinned release (default below; bump deliberately)
#   TRIVY_INSTALL_DIR   — where to drop the binary
set -euo pipefail

# shellcheck source=template-scripts/lib-common.sh
source "$(dirname "$0")/../lib-common.sh"

TRIVY_VERSION="${TRIVY_VERSION:-0.59.1}"
TRIVY_INSTALL_DIR="${TRIVY_INSTALL_DIR:-$HOME/.local/bin}"

if command -v trivy >/dev/null 2>&1; then
    echo "trivy already installed: $(trivy --version 2>/dev/null | awk '/Version:/ {print $2; exit}' || echo unknown)"
    exit 0
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
    x86_64) arch_suffix=64bit ;;
    aarch64|arm64) arch_suffix=ARM64 ;;
    *) echo "Unsupported arch: $arch"; exit 1 ;;
esac

case "$os" in
    darwin) os_suffix=macOS ;;
    linux) os_suffix=Linux ;;
    *) echo "Unsupported OS: $os"; exit 1 ;;
esac

if [ "$os" = "darwin" ] && command -v brew >/dev/null 2>&1; then
    echo "Installing trivy via brew."
    brew install trivy
    exit 0
fi

tarball="trivy_${TRIVY_VERSION}_${os_suffix}-${arch_suffix}.tar.gz"
base="https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}"

install_release_tarball trivy "$TRIVY_INSTALL_DIR" \
    "${base}/${tarball}" "${base}/trivy_${TRIVY_VERSION}_checksums.txt" "$tarball"
