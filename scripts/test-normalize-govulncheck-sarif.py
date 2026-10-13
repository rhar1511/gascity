#!/usr/bin/env python3
"""Regression tests for duplicate stacks rejected by Code Scanning."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("normalize-govulncheck-sarif.py")
spec = importlib.util.spec_from_file_location("normalizer", SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def report(stacks):
    return {"version": "2.1.0", "$schema": "preserved", "runs": [
        {"tool": {"driver": {"name": "govulncheck"}}, "results": [
            {"ruleId": "GO-test", "level": "error", "stacks": stacks, "message": {"text": "preserved"}},
            {"ruleId": "GO-note", "level": "note", "properties": {"unknown": True}},
        ], "properties": {"unknown": [1, 2]}}
    ]}


class NormalizerTests(unittest.TestCase):
    def test_duplicate_stack_preserves_findings_and_distinct_traces(self):
        stack = {"message": {"text": "trace"}, "frames": [{"module": "app", "properties": {"line": 1}}]}
        different = copy.deepcopy(stack)
        different["frames"][0]["properties"]["line"] = 2
        before = report([stack, copy.deepcopy(stack), different, copy.deepcopy(stack)])
        original = copy.deepcopy(before)
        after, changes = module.normalize(before)
        expected = copy.deepcopy(before)
        expected["runs"][0]["results"][0]["stacks"] = [stack, different]
        self.assertEqual(after, expected)
        self.assertEqual(before, original)
        self.assertEqual(changes, [{"run": 0, "result": 0, "removed": 2}])
        self.assertEqual(module.normalize(after), (after, []))

    def test_property_order_does_not_make_duplicate_distinct(self):
        before = report([{"message": {"text": "a"}, "frames": []},
                         {"frames": [], "message": {"text": "a"}}])
        after, changes = module.normalize(before)
        self.assertEqual(len(after["runs"][0]["results"][0]["stacks"]), 1)
        self.assertEqual(changes[0]["removed"], 1)

    def test_empty_missing_stacks_and_multiple_runs(self):
        before = report([])
        before["runs"].append(report([{"frames": []}, {"frames": []}])["runs"][0])
        before["runs"].append({"tool": {"driver": {"name": "other"}}})
        after, changes = module.normalize(before)
        self.assertEqual(after["runs"][0], before["runs"][0])
        self.assertEqual(after["runs"][2], before["runs"][2])
        self.assertEqual(changes, [{"run": 1, "result": 0, "removed": 1}])
        self.assertEqual(module.normalize({"version": "2.1.0", "runs": []}),
                         ({"version": "2.1.0", "runs": []}, []))

    def test_malformed_structures_refuse(self):
        cases = [None, {}, {"version": "2.1.0", "runs": {}},
                 {"version": "2.1.0", "runs": [None]},
                 {"version": "2.1.0", "runs": [{"results": {}}]},
                 report(None), report([None])]
        for value in cases:
            with self.subTest(value=value), self.assertRaises(ValueError):
                module.normalize(value)

    def test_cli_preserves_raw_and_records_hashes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, destination, summary = [root / name for name in ["raw.json", "upload.json", "summary.json"]]
            raw = json.dumps(report([{"frames": []}, {"frames": []}])).encode()
            source.write_bytes(raw)
            subprocess.run([sys.executable, str(SCRIPT), str(source), str(destination), str(summary)],
                           check=True, capture_output=True)
            self.assertEqual(source.read_bytes(), raw)
            receipt = json.loads(summary.read_text())
            self.assertEqual(receipt["raw_sha256"], hashlib.sha256(raw).hexdigest())
            self.assertEqual(receipt["normalized_sha256"], hashlib.sha256(destination.read_bytes()).hexdigest())
            self.assertEqual(receipt["duplicate_stacks_removed"], 1)

    def test_cli_refuses_alias_and_malformed_without_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, destination, summary = [root / name for name in ["raw.json", "upload.json", "summary.json"]]
            source.write_text("not JSON")
            for target in [source, destination]:
                result = subprocess.run([sys.executable, str(SCRIPT), str(source), str(target), str(summary)],
                                        capture_output=True)
                self.assertNotEqual(result.returncode, 0)
            self.assertEqual(source.read_text(), "not JSON")
            self.assertFalse(destination.exists())
            self.assertFalse(summary.exists())


if __name__ == "__main__":
    unittest.main()
