#!/usr/bin/env python3
"""
check-prometheus-rule-crds.py

Extracts spec.groups from each PrometheusRule CRD passed on argv, writes the
bare-groups form to a tempfile, and runs `promtool check rules` against it.
Apl-core's kube-prometheus-stack consumes the wrapped CRD form; promtool only
understands the bare-groups format.

Usage:
  python3 template-scripts/linting-and-validation/check-prometheus-rule-crds.py <file> [<file> ...]

Exit codes:
  0 — every file parses and promtool reports SUCCESS
  1 — one or more files fail validation
"""

from __future__ import annotations

import subprocess
import sys
import tempfile
from pathlib import Path

try:
    import yaml
except ModuleNotFoundError:
    sys.exit("PyYAML is required: pip install pyyaml")


def check_rule_crd(path: Path) -> int:
    try:
        doc = yaml.safe_load(path.read_text())
    except yaml.YAMLError as exc:
        print(f"::error file={path}::Failed to parse YAML: {exc}")
        return 1

    if not isinstance(doc, dict) or doc.get("kind") != "PrometheusRule":
        print(f"::error file={path}::Not a PrometheusRule CRD (kind={doc.get('kind') if isinstance(doc, dict) else type(doc).__name__})")
        return 1

    groups = (doc.get("spec") or {}).get("groups")
    if not groups:
        print(f"::error file={path}::PrometheusRule has no spec.groups")
        return 1

    bare = yaml.safe_dump({"groups": groups}, default_flow_style=False)
    with tempfile.NamedTemporaryFile("w", suffix=".rules.yml", delete=False) as tmp:
        tmp.write(bare)
        tmp_path = tmp.name

    try:
        subprocess.check_call(["promtool", "check", "rules", tmp_path])
    except subprocess.CalledProcessError:
        print(f"::error file={path}::promtool rejected rules")
        return 1
    finally:
        Path(tmp_path).unlink(missing_ok=True)
    return 0


def main() -> int:
    if len(sys.argv) < 2:
        sys.exit("Usage: check-prometheus-rule-crds.py <file> [<file> ...]")
    errors = sum(check_rule_crd(Path(arg)) for arg in sys.argv[1:])
    if errors:
        print(f"\n{errors} PrometheusRule file(s) failed validation.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
