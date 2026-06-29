#!/usr/bin/env bash
# Deploy a Spin app to Fermyon Wasm Functions on Akamai. App-agnostic: deploys the
# manifest you pass. Extend for multi-app stacks or app-suffix handling as needed.
set -euo pipefail
manifest="spin.toml"; suffix=""
while [ $# -gt 0 ]; do case "$1" in
  --manifest) manifest="$2"; shift 2;;
  --app-suffix) suffix="$2"; shift 2;;
  *) echo "unknown arg: $1" >&2; exit 2;;
esac; done
echo "Deploying ${manifest} (suffix='${suffix}') to Fermyon/Akamai…"
spin cloud deploy -f "$manifest"
