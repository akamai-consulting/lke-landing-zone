# shellcheck shell=bash
#
# Common shell helpers shared across template-scripts/.
# Source this file; it defines functions but does not execute anything.

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# step — print a bold section header. Used by the multi-stage check scripts
# (instance-test.sh, scaffold-render-check.sh) to delimit their phases.
step() { printf '\n\033[1m── %s\033[0m\n' "$1"; }

# fail — record a non-fatal failure and keep going. Accumulates into the
# caller's `FAILED` global (the caller declares `FAILED=0` and inspects it at the
# end) so a check script can report every problem in one run rather than aborting
# on the first. Emits a GitHub Actions error annotation.
# shellcheck disable=SC2034  # FAILED is the caller's accumulator, read there not here.
fail() { echo "::error::$1"; FAILED=1; }

# usage — print the calling script's own header-comment block as help text:
# everything from line 2 down to the `set -euo pipefail` line, with the leading
# `# ` stripped. Relies on $0 being the calling script — sourcing this file does
# not change $0, so each caller prints ITS OWN header. Convention to keep this
# working: put the usage docs in the header comment and terminate them with the
# `set -euo pipefail` line (as the template-scripts all do).
usage() { sed -n '2,/^set -euo/{/^set -euo/d;s/^# \{0,1\}//;p;}' "$0"; }

# detect_tf — print the Terraform binary to use: an explicit $TF override if set,
# else terraform, else tofu, else nothing. Callers decide what an empty result
# means — hard error, graceful skip, or a default — so this handles detection
# only, not the fallback policy (the three TF roots each want different
# behaviour). CI lint containers ship only tofu (the OpenTofu dev alias).
detect_tf() {
  if [[ -n "${TF:-}" ]]; then printf '%s' "$TF"; return 0; fi
  if command -v terraform >/dev/null 2>&1; then printf 'terraform'; return 0; fi
  if command -v tofu >/dev/null 2>&1; then printf 'tofu'; return 0; fi
}

# install_release_tarball — download a release tarball, verify it against the
# release's checksums file, and install a single binary from it (no sudo). Shared
# by the install-*.sh scripts, whose only real differences are the OS/arch naming
# each project uses to build the tarball name — the caller computes those and
# passes the finished URLs and filename here.
#
# Usage:
#   install_release_tarball NAME INSTALL_DIR TARBALL_URL CHECKSUMS_URL \
#                           TARBALL_FILE [BIN_IN_TARBALL]
# BIN_IN_TARBALL defaults to NAME.
install_release_tarball() {
  local name="$1" dir="$2" tarball_url="$3" checksums_url="$4"
  local tarball_file="$5" bin="${6:-$1}"

  mkdir -p "$dir"
  # Cleanup is registered on EXIT (not RETURN) so it also fires when a `die` or a
  # caller's set -e aborts mid-function. The temp dir is a namespaced global, not
  # a `local`: an EXIT trap runs at shell exit, by which point a local would be
  # out of scope (and tripping set -u). The :- default keeps set -u happy.
  _libcommon_tmp=$(mktemp -d)
  trap 'rm -rf "${_libcommon_tmp:-}"' EXIT

  echo "Downloading ${tarball_file} ..."
  # --retry rides out transient GitHub release-CDN blips (429/503/timeouts/conn
  # refused) that would otherwise fail a healthy install on a one-off hiccup. NOT
  # --retry-all-errors: a real 4xx (e.g. 404 wrong URL) should fail fast, not retry.
  # The sha256 check below still guards integrity, so a retried download cannot mask
  # corruption.
  curl -fsSL --retry 5 --retry-delay 2 --retry-connrefused "$tarball_url"   -o "${_libcommon_tmp}/pkg.tar.gz"
  curl -fsSL --retry 5 --retry-delay 2 --retry-connrefused "$checksums_url" -o "${_libcommon_tmp}/checksums.txt"

  local expected
  expected=$(awk -v t="${tarball_file}" '$2 == t {print $1}' "${_libcommon_tmp}/checksums.txt")
  [ -n "$expected" ] || die "no checksum entry for ${tarball_file} in checksums.txt — refusing to install"

  # Verify explicitly (don't lean on the caller's set -e to abort on a bad hash):
  # `if ! ...` suppresses set -e for the check itself so the failure surfaces as
  # our own message-and-die rather than a bare non-zero exit.
  local -a hashcmd
  if command -v sha256sum >/dev/null 2>&1; then hashcmd=(sha256sum -c -); else hashcmd=(shasum -a 256 -c -); fi
  if ! echo "${expected}  ${_libcommon_tmp}/pkg.tar.gz" | "${hashcmd[@]}"; then
    die "checksum verification failed for ${tarball_file} — refusing to install"
  fi

  tar -xzf "${_libcommon_tmp}/pkg.tar.gz" -C "${_libcommon_tmp}" "$bin"
  install -m 0755 "${_libcommon_tmp}/${bin}" "${dir}/${bin}"
  echo "Installed ${name} to ${dir}/${bin}"

  case ":${PATH}:" in
    *":${dir}:"*) ;;
    *) echo "NOTE: ${dir} is not on your PATH — add it (e.g. in your shell rc) so ${name} is callable." ;;
  esac
}
