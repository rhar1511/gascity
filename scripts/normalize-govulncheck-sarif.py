#!/usr/bin/env python3
"""Remove identical duplicate SARIF stacks without changing findings."""
import argparse
import copy
import hashlib
import json
from pathlib import Path


def normalize(report):
    """Return a separate report and the duplicate counts for each result."""
    if not isinstance(report, dict) or report.get("version") != "2.1.0":
        raise ValueError("expected a SARIF 2.1.0 object")
    runs = report.get("runs")
    if not isinstance(runs, list):
        raise ValueError("SARIF runs must be an array")
    result = copy.deepcopy(report)
    changes = []
    for run_index, run in enumerate(result["runs"]):
        if not isinstance(run, dict):
            raise ValueError("SARIF run must be an object")
        findings = run.get("results", [])
        if not isinstance(findings, list):
            raise ValueError("SARIF results must be an array")
        for result_index, finding in enumerate(findings):
            if not isinstance(finding, dict):
                raise ValueError("SARIF result must be an object")
            if "stacks" not in finding:
                continue
            stacks = finding["stacks"]
            if not isinstance(stacks, list) or any(not isinstance(s, dict) for s in stacks):
                raise ValueError("SARIF stacks must be an array of objects")
            unique, seen = [], set()
            for stack in stacks:
                key = json.dumps(stack, sort_keys=True, separators=(",", ":"), allow_nan=False)
                if key not in seen:
                    seen.add(key)
                    unique.append(stack)
            removed = len(stacks) - len(unique)
            if removed:
                finding["stacks"] = unique
                changes.append({"run": run_index, "result": result_index, "removed": removed})
    return result, changes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("destination", type=Path)
    parser.add_argument("summary", type=Path)
    args = parser.parse_args()
    paths = [args.source, args.destination, args.summary]
    if len({p.resolve() for p in paths}) != 3:
        parser.error("source, destination and summary must be separate files")
    # Refuse hard-link aliases too: the original scanner artifact stays untouched.
    if any(a.exists() and b.exists() and a.samefile(b)
           for i, a in enumerate(paths) for b in paths[i + 1:]):
        parser.error("source, destination and summary must not alias")
    raw = args.source.read_bytes()
    normalized, changes = normalize(json.loads(raw))
    output = (json.dumps(normalized, indent=2, allow_nan=False) + "\n").encode()
    summary = {
        "raw_sha256": hashlib.sha256(raw).hexdigest(),
        "normalized_sha256": hashlib.sha256(output).hexdigest(),
        "raw_bytes": len(raw), "normalized_bytes": len(output),
        "duplicate_stacks_removed": sum(change["removed"] for change in changes),
        "changes": changes,
    }
    args.destination.write_bytes(output)
    args.summary.write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary))


if __name__ == "__main__":
    main()
