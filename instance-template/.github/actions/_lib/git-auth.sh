#!/usr/bin/env bash
# Shared git auth for `terraform init` against published first-party modules.
#
# First-party Terraform modules are consumed as
#   git::ssh://git@<host>/<org>/<repo>.git//terraform-modules/<name>?ref=<tag>
# (the published, SemVer-tagged reuse units — see terraform-modules/RELEASING.md).
# In CI:
#   * `terraform init` clones each git:: module source into .terraform/modules/<name>.
#     In container jobs the workspace is bind-mounted with the runner user's
#     ownership, which differs from the UID git runs as inside the TF container, so
#     go-getter's clone trips git's "detected dubious ownership" guard
#     (git >= 2.35.2) and init fails. Trust every path for this ephemeral CI user
#     so the module clones proceed. '*' covers the unpredictable nested module
#     dirs without enumerating them.
#   * The container has no SSH key for <host>, so the SSH clone would fail with
#     "Permission denied (publickey)" / "Failed to download module". Rewrite those
#     SSH fetches to authenticated HTTPS using GH_TOKEN — the same credential
#     actions/checkout uses (contents:read). The host is derived from
#     GITHUB_SERVER_URL so this follows the repo if it ever moves hosts. Modules
#     consumed via relative paths are unaffected (no fetch happens).
#
# Sourced (not exec'd) by the terraform-init and fetch-kubeconfig composite
# actions via "$GITHUB_ACTION_PATH/../_lib/git-auth.sh" so the logic lives once.
# Expects GH_TOKEN and GITHUB_SERVER_URL in the environment.

git config --global --add safe.directory '*'

if [ -n "${GH_TOKEN:-}" ]; then
  host="${GITHUB_SERVER_URL#https://}"
  git config --global \
    url."https://x-access-token:${GH_TOKEN}@${host}/".insteadOf \
    "ssh://git@${host}/"
fi
