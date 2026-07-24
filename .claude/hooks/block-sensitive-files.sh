#!/usr/bin/env bash
# PreToolUse guard (Write|Edit): refuse to write key-material files into the
# tree. Mirrors the pre-commit secrets guard in template-scripts/hooks/pre-commit
# (*.pem / *.der / *.key), one layer earlier — at edit time instead of commit
# time. Exit 2 blocks the tool call; anything else lets it proceed.
set -euo pipefail

command -v jq >/dev/null 2>&1 || exit 0
file=$(jq -r '.tool_input.file_path // empty' 2>/dev/null || true)
[ -n "$file" ] || exit 0

case "$file" in
  *.pem|*.der|*.key)
    echo "Blocked: '$file' matches the key-material patterns (*.pem/*.der/*.key)." >&2
    echo "The pre-commit hook (template-scripts/hooks/pre-commit) rejects these at commit time; do not write them into the tree at all." >&2
    exit 2
    ;;
esac
exit 0
