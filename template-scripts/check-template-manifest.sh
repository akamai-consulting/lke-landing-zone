#!/usr/bin/env bash
# check-template-manifest.sh — keep .template-manifest honest, and classify paths.
#
# The manifest (instance-template/.template-manifest, shipped to instances as
# .template-manifest) maps every scaffold file to managed / merge / owned so an
# update tool can apply template changes without clobbering instance-local edits.
# This script is the guard that keeps that map complete, plus the classifier the
# update/drift tooling calls.
#
# Scaffold root is auto-detected: instance-template/ when run from the template
# repo, else the current dir (an instance, where the manifest sits at the root).
#
# Usage:
#   template-scripts/check-template-manifest.sh                 # validate: every scaffold
#                                                      #   file matches a rule (CI gate)
#   template-scripts/check-template-manifest.sh --classify PATH # print the class of PATH
#   template-scripts/check-template-manifest.sh --list CLASS    # list scaffold files in CLASS
#   -h, --help
#
# Exit codes: 0 ok; 1 one or more scaffold files match no rule (or usage error).

set -euo pipefail

# shellcheck source=template-scripts/lib-common.sh
source "$(dirname "$0")/lib-common.sh"

MODE="check"; ARG=""
case "${1:-}" in
  -h|--help)  usage; exit 0 ;;
  --classify) MODE="classify"; ARG="${2:-}"; [ -n "$ARG" ] || { echo "error: --classify needs a PATH" >&2; exit 1; } ;;
  --list)     MODE="list";     ARG="${2:-}"; [ -n "$ARG" ] || { echo "error: --list needs a CLASS" >&2; exit 1; } ;;
  "")         MODE="check" ;;
  *)          echo "error: unknown option: $1 (see --help)" >&2; exit 1 ;;
esac

# Locate the manifest + its scaffold root.
if [ -f instance-template/.template-manifest ]; then
  ROOT="instance-template"
elif [ -f .template-manifest ]; then
  ROOT="."
else
  echo "error: .template-manifest not found (looked in instance-template/ and .)" >&2
  exit 1
fi

MODE="$MODE" ARG="$ARG" ROOT="$ROOT" python3 - <<'PY'
import os, re, sys

mode = os.environ["MODE"]; arg = os.environ["ARG"]; root = os.environ["ROOT"]
manifest = os.path.join(root, ".template-manifest") if root != "." else ".template-manifest"

# Parse rules: list of (cls, glob) in file order; last match wins.
rules = []
with open(manifest) as f:
    for line in f:
        s = line.strip()
        if not s or s.startswith("#"):
            continue
        parts = s.split(None, 1)
        if len(parts) != 2 or parts[0] not in ("managed", "merge", "owned"):
            sys.exit(f"manifest: bad rule (expected `<managed|merge|owned>  <glob>`): {s!r}")
        rules.append((parts[0], parts[1]))

def glob_to_re(g):
    # `/`-aware: ** crosses dirs, * and ? do not.
    i, out, n = 0, ["(?s:"], len(g)
    while i < n:
        c = g[i]
        if g[i:i+2] == "**":
            out.append(".*"); i += 2
            if i < n and g[i] == "/":   # `**/` also matches zero dirs
                out.append("/?"); i += 1
        elif c == "*":
            out.append("[^/]*"); i += 1
        elif c == "?":
            out.append("[^/]"); i += 1
        else:
            out.append(re.escape(c)); i += 1
    out.append(")$")
    return re.compile("".join(out))

compiled = [(cls, glob_to_re(g), g) for cls, g in rules]

def classify(relpath):
    hit = None
    for cls, rx, _ in compiled:           # last match wins
        if rx.match(relpath):
            hit = cls
    return hit

def scaffold_files():
    base = root
    for dirpath, dirnames, filenames in os.walk(base):
        dirnames[:] = [d for d in dirnames if d not in (".git", ".terraform")]
        for fn in filenames:
            full = os.path.join(dirpath, fn)
            yield os.path.relpath(full, base)

if mode == "classify":
    cls = classify(arg)
    if cls is None:
        print(f"{arg}: UNCLASSIFIED", file=sys.stderr); sys.exit(1)
    print(cls); sys.exit(0)

if mode == "list":
    if arg not in ("managed", "merge", "owned"):
        sys.exit(f"error: unknown class {arg!r} (managed|merge|owned)")
    for rel in sorted(scaffold_files()):
        if classify(rel) == arg:
            print(rel)
    sys.exit(0)

# mode == check: every scaffold file must match a rule.
unclassified = sorted(rel for rel in scaffold_files() if classify(rel) is None)
counts = {"managed": 0, "merge": 0, "owned": 0}
for rel in scaffold_files():
    c = classify(rel)
    if c:
        counts[c] += 1
if unclassified:
    print(f"::error::{len(unclassified)} scaffold file(s) match no rule in {manifest}:", file=sys.stderr)
    for u in unclassified:
        print(f"  - {u}", file=sys.stderr)
    print("Add a rule for each (managed | merge | owned) — see the header in "
          f"{manifest}.", file=sys.stderr)
    sys.exit(1)
print(f"template-manifest: OK — managed={counts['managed']} "
      f"merge={counts['merge']} owned={counts['owned']} "
      f"({sum(counts.values())} files, all classified)")
PY
