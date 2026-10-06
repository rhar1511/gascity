#!/usr/bin/env python3
"""Run an explicit external consumer against the disposable real Go fixture."""

import argparse
import json
import os
from pathlib import Path
import selectors
import subprocess
import sys
import tempfile
import time
from urllib.request import Request, urlopen


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runner", type=Path, required=True)
    parser.add_argument("--bounded-go", type=Path, required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    command = [str(args.bounded_go.resolve(strict=True)), "test",
               "-tags=cross_language_harness", "./internal/api", "-run",
               "^TestCrossLanguageRetirementHarnessServe$", "-count=1", "-v", "-timeout=150s"]
    env = dict(os.environ, GC_RETIREMENT_WIRE_HARNESS="1", PYTHONDONTWRITEBYTECODE="1")
    process = subprocess.Popen(command, cwd=root, env=env, stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, text=True)
    plan = None
    logs = []
    result = 1
    try:
        selector = selectors.DefaultSelector()
        selector.register(process.stdout, selectors.EVENT_READ)
        deadline = time.monotonic() + 90
        try:
            while time.monotonic() < deadline:
                if not selector.select(timeout=1):
                    if process.poll() is not None:
                        break
                    continue
                line = process.stdout.readline()
                if not line:
                    break
                logs.append(line)
                if line.startswith("GC_RETIREMENT_PLAN="):
                    plan = json.loads(line.split("=", 1)[1])
                    break
        finally:
            selector.close()
        if plan is None:
            raise RuntimeError("fixture did not publish its plan: " + "".join(logs))
        with tempfile.TemporaryDirectory(prefix="retirement-wire-plan-", dir="/tmp/opencode") as scratch:
            path = Path(scratch) / "plan.json"
            path.write_text(json.dumps(plan, sort_keys=True), encoding="utf-8")
            completed = subprocess.run([sys.executable, "-B", str(args.runner.resolve(strict=True)),
                                        "--plan", str(path)], cwd=scratch, env=env, timeout=90)
            result = completed.returncode
    finally:
        if plan is not None:
            stop = Request(plan["cases"][0]["base_url"] + "/__fixture/stop", data=b"{}",
                           method="POST", headers={"Content-Type": "application/json"})
            try:
                with urlopen(stop, timeout=5) as response:
                    response.read()
            except OSError:
                result = 1
        try:
            rest, _ = process.communicate(timeout=15)
        except subprocess.TimeoutExpired:
            process.terminate()
            rest, _ = process.communicate(timeout=5)
            result = 1
        if process.returncode != 0:
            sys.stderr.write("".join(logs) + rest)
            result = 1
    return result


if __name__ == "__main__":
    raise SystemExit(main())
