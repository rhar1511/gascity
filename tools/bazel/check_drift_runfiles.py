#!/usr/bin/env python3
"""Check that the drift integration fixture can read its Bazel source inputs."""

from __future__ import annotations

import json
import re
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
FIXTURE = ROOT / "test/integration/start_drift_test.go"


def source_inputs() -> list[str]:
    fixture = FIXTURE.read_text()
    _, separator, materializer = fixture.partition("func materializeDriftBuildSource(")
    if not separator:
        raise ValueError("drift source materializer not found")
    match = re.search(r"for _, name := range \[\]string\{([^}]*)\}", materializer)
    if match is None:
        raise ValueError("drift source input list not found")
    names = json.loads("[" + match.group(1) + "]")
    if not names or len(names) != len(set(names)):
        raise ValueError("drift source input list is empty or contains duplicates")
    return names


def query(expression: str) -> set[str]:
    result = subprocess.run(
        ["bazel", "query", expression, "--output=label"],
        cwd=ROOT,
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode:
        raise RuntimeError(result.stderr.strip() or f"bazel query exited {result.returncode}")
    return set(result.stdout.splitlines())


def source_path(label: str) -> str | None:
    if not label.startswith("//") or ":" not in label:
        return None
    package, target = label[2:].split(":", 1)
    return f"{package}/{target}" if package else target


def main() -> int:
    names = source_inputs()
    data = query("labels(data, //test/integration:integration_test)")
    if "//:repo_source_tree" not in data:
        raise ValueError("integration test does not declare //:repo_source_tree as data")
    source_labels = query('kind("source file", deps(//:repo_source_tree))')
    available = {
        path for label in source_labels | data
        if (path := source_path(label)) is not None
    }
    missing = [
        name for name in names
        if not (
            any(path.startswith(name + "/") for path in available)
            if (ROOT / name).is_dir()
            else name in available
        )
    ]
    if missing:
        print("Drift build inputs missing from Bazel runfiles: " + ", ".join(missing), file=sys.stderr)
        return 1
    print(f"Drift build inputs present in Bazel runfiles: {', '.join(names)}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, RuntimeError, ValueError) as error:
        print(f"Drift runfiles check failed: {error}", file=sys.stderr)
        sys.exit(1)
