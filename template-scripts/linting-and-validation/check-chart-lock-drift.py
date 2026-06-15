#!/usr/bin/env python3
"""
check-chart-lock-drift.py

Verifies that every committed Chart.lock file matches the dependency declarations
in the corresponding Chart.yaml. Exits non-zero if any dependency name, version,
or repository differs — meaning Chart.yaml was updated without re-running
`helm dependency update`.

Usage:
  python3 template-scripts/linting-and-validation/check-chart-lock-drift.py <chart-dir> [<chart-dir> ...]

Exit codes:
  0 — all lock files match their Chart.yaml
  1 — one or more charts have lock drift or missing lock files
"""

from __future__ import annotations

import sys
from pathlib import Path

try:
    import yaml
except ModuleNotFoundError:
    sys.exit("PyYAML is required: pip install pyyaml")


def check_chart(chart_dir: Path) -> int:
    chart_yaml_path = chart_dir / "Chart.yaml"
    chart_lock_path = chart_dir / "Chart.lock"

    if not chart_yaml_path.exists():
        print(f"::error::No Chart.yaml found in {chart_dir}")
        return 1

    chart_data = yaml.safe_load(chart_yaml_path.read_text()) or {}
    declared_deps = {
        d["name"]: d for d in (chart_data.get("dependencies") or [])
    }

    if not declared_deps:
        print(f"  skip (no dependencies): {chart_dir}")
        return 0

    if not chart_lock_path.exists():
        print(
            f"::error file={chart_yaml_path}::{chart_dir}: Chart.lock is missing. "
            f"Run `helm dependency update {chart_dir}` and commit the result."
        )
        return 1

    lock_data = yaml.safe_load(chart_lock_path.read_text()) or {}
    locked_deps = {
        d["name"]: d for d in (lock_data.get("dependencies") or [])
    }

    errors = 0
    for name, declared in sorted(declared_deps.items()):
        if name not in locked_deps:
            print(
                f"::error file={chart_lock_path}::{chart_dir}: dependency '{name}' "
                f"is declared in Chart.yaml but missing from Chart.lock. "
                f"Run `helm dependency update {chart_dir}`."
            )
            errors += 1
            continue

        locked = locked_deps[name]
        for field in ("version", "repository"):
            if declared.get(field) != locked.get(field):
                print(
                    f"::error file={chart_lock_path}::{chart_dir}: dependency '{name}' "
                    f"{field} mismatch — Chart.yaml: {declared.get(field)!r}, "
                    f"Chart.lock: {locked.get(field)!r}. "
                    f"Run `helm dependency update {chart_dir}`."
                )
                errors += 1

    for name in sorted(set(locked_deps) - set(declared_deps)):
        print(
            f"::warning file={chart_lock_path}::{chart_dir}: Chart.lock contains "
            f"'{name}' which is not in Chart.yaml — stale lock entry."
        )

    if errors == 0:
        print(f"  ok: {chart_dir}")
    return errors


def main() -> int:
    if len(sys.argv) < 2:
        sys.exit(f"Usage: {sys.argv[0]} <chart-dir> [<chart-dir> ...]")

    total = 0
    for arg in sys.argv[1:]:
        total += check_chart(Path(arg))

    if total:
        print(f"\n{total} Chart.lock drift error(s) found.")
        return 1
    print("\nAll Chart.lock files are in sync with Chart.yaml.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
