#!/usr/bin/env python3
"""
validate-argocd-rendered-apps.py

Validates semantic properties of the rendered ArgoCD Application resources that
schema checks do not catch (e.g. duplicate Helm parameters). The landing-zone
template generates its Applications from the argo-bootstrap-apps chart, so this
reads the rendered chart output ($RENDER_DIR, produced by
template-scripts/ci/render-charts.sh) rather than kustomize-building apl-values overlays.

Exit codes:
    0 - rendered Applications passed validation
    1 - one or more rendered Applications failed validation
"""

from __future__ import annotations

import os
import re
import sys
from collections import Counter
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent.parent
RENDER_DIR = os.environ.get("RENDER_DIR", "rendered")


def load_documents() -> list[tuple[str, str]]:
    """Return [(source_file, yaml_document), ...] for every rendered manifest."""
    docs: list[tuple[str, str]] = []
    for f in sorted((REPO_ROOT / RENDER_DIR).glob("**/*.yaml")):
        text = f.read_text(errors="replace")
        rel = str(f.relative_to(REPO_ROOT))
        for document in re.split(r"^---\s*$", text, flags=re.MULTILINE):
            docs.append((rel, document))
    return docs


def yaml_scalar(document: str, key: str) -> str | None:
    match = re.search(rf"^\s*{re.escape(key)}:\s+(.+?)\s*$", document, re.MULTILINE)
    if match is None:
        return None
    return match.group(1).strip('"')


def helm_parameter_names(document: str) -> list[str]:
    names: list[str] = []
    lines = document.splitlines()
    for index, line in enumerate(lines):
        if not re.match(r"^\s+parameters:\s*$", line):
            continue

        base_indent = len(line) - len(line.lstrip())
        for child in lines[index + 1 :]:
            if not child.strip():
                continue
            indent = len(child) - len(child.lstrip())
            if indent <= base_indent:
                break
            name_match = re.match(r"^\s*-\s+name:\s+(.+?)\s*$", child)
            if name_match is not None:
                names.append(name_match.group(1).strip('"'))
    return names


def main() -> int:
    documents = load_documents()
    if not documents:
        print(
            f"::error::no rendered manifests under {RENDER_DIR}/ — "
            f"run 'make render-charts' first"
        )
        return 1

    errors = 0
    apps = 0
    for source, document in documents:
        if yaml_scalar(document, "kind") != "Application":
            continue
        apps += 1
        app_name = yaml_scalar(document, "name") or "<unknown>"
        counts = Counter(helm_parameter_names(document))
        duplicates = sorted(name for name, count in counts.items() if count > 1)
        for name in duplicates:
            print(
                f"::error file={source}::Rendered Application "
                f"'{app_name}' has duplicate Helm parameter '{name}'"
            )
            errors += 1

    if errors:
        print(f"\n{errors} rendered ArgoCD Application validation error(s).")
        return 1
    print(f"\n{apps} rendered ArgoCD Application(s) passed semantic validation.")
    return 0


if __name__ == "__main__":
    sys.exit(main())