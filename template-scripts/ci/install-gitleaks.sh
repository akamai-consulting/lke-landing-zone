#!/usr/bin/env bash
# Install gitleaks for `make gitleaks` (the CI secret-scan gate and local runs).
#
# macOS + brew: `brew install gitleaks` (matches the repo's install-tools convention).
# Anywhere else: SHA-verified tarball from gitleaks' GitHub release, into
# $GITLEAKS_INSTALL_DIR (default $HOME/.local/bin), no sudo.
#
# Override:
#   GITLEAKS_VERSION       — pinned release (default below; bump deliberately)
#   GITLEAKS_INSTALL_DIR   — where to drop the binary
set -euo pipefail

# shellcheck source=template-scripts/lib-common.sh
source "$(dirname "$0")/../lib-common.sh"

GITLEAKS_VERSION="${GITLEAKS_VERSION:-8.30.1}"
GITLEAKS_INSTALL_DIR="${GITLEAKS_INSTALL_DIR:-$HOME/.local/bin}"

if command -v gitleaks >/dev/null 2>&1; then
    echo "gitleaks already installed: $(gitleaks version 2>/dev/null || echo unknown)"
    exit 0
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
    x86_64) arch_suffix=x64 ;;
    aarch64|arm64) arch_suffix=arm64 ;;
    *) echo "Unsupported arch: $arch"; exit 1 ;;
esac

case "$os" in
    darwin) os_suffix=darwin ;;
    linux) os_suffix=linux ;;
    *) echo "Unsupported OS: $os"; exit 1 ;;
esac

if [ "$os" = "darwin" ] && command -v brew >/dev/null 2>&1; then
    echo "Installing gitleaks via brew."
    brew install gitleaks
    exit 0
fi

tarball="gitleaks_${GITLEAKS_VERSION}_${os_suffix}_${arch_suffix}.tar.gz"
base="https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}"

install_release_tarball gitleaks "$GITLEAKS_INSTALL_DIR" \
    "${base}/${tarball}" "${base}/gitleaks_${GITLEAKS_VERSION}_checksums.txt" "$tarball"
