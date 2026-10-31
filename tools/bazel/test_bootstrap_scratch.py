"""Pinned rules_go bootstrap cache/scratch boundary. Owner: rcity-8ytr.

The Apache-2.0 upstream fixture is binary_wrapper.sh from rules_go v0.63.0:
https://github.com/bazel-contrib/rules_go/blob/v0.63.0/go/private/rules/binary_wrapper.sh
Its SHA is checked before patching. Real Bash/patch/filesystem are exercised;
GO_BINARY is an inert recorder that rejects clean and never invokes real Go.
This owns the Unix wrapper boundary, not Bazel containment or native workers.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import subprocess
import sys
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[2]
FIXTURE = Path(__file__).parent / "testdata/rules_go_binary_wrapper_0_63_0.sh.txt"
PATCH = REPO / "third_party/patches/rules_go_private_bootstrap_scratch.patch"
FIXTURE_SHA = "24e5f7910319b9aad22489369b8ea8d3b11b9a5745bb7dbce5ae8439c00b175d"
EXEC_TIMEOUT = 20  # Hang detector; successful cases return immediately.

RECORDER = r"""
import json,os,signal,sys
from pathlib import Path
kind=sys.argv[1]
args=sys.argv[2:]
log=Path(os.environ['FIXTURE_CALLS'])
rows=[json.loads(x) for x in log.read_text().splitlines()] if log.exists() else []
entry={'tool':kind,'argv':args,'cwd':os.getcwd(),'env':{k:os.environ.get(k) for k in ['HOME','GOCACHE','GOPATH','GOTMPDIR','TMPDIR']}}
with log.open('a') as stream:stream.write(json.dumps(entry)+'\n')
mode=os.environ['FIXTURE_MODE']
if kind!='go':
 if mode=='fail-'+kind or kind=='rm' and mode=='fail-first-and-rm':raise SystemExit({'mktemp':71,'mkdir':72,'rm':73}[kind])
 real=os.environ['FIXTURE_REAL_'+kind.upper()]
 os.execv(real,[real,*args])
if not args or args[0]!='build':raise SystemExit(97)
count=1+sum(x['tool']=='go' and x['argv'][0]=='build' for x in rows)
if mode=='fail-first' and count==1 or mode=='fail-second' and count==2 or mode=='fail-first-and-rm' and count==1:raise SystemExit(37)
if mode=='interrupt' and count==1:
 signal.signal(signal.SIGTERM,lambda *_:sys.exit(143))
 print('INERT_READY',flush=True)
 signal.pause()
for key in ['GOCACHE','GOPATH','GOTMPDIR','TMPDIR']:
 value=os.environ.get(key)
 if value and value.startswith(os.environ['FIXTURE_ROOT']+os.sep):
  directory=Path(value);directory.mkdir(parents=True,exist_ok=True)
  (directory/'inert-object').write_text('not a Go cache object')
output=Path(args[args.index('-o')+1]);output.parent.mkdir(parents=True,exist_ok=True);output.write_text('inert-output-'+str(count))
"""

class BootstrapScratchTest(unittest.TestCase):
    def setUp(self):
        self.assertEqual(hashlib.sha256(FIXTURE.read_bytes()).hexdigest(), FIXTURE_SHA)
        self.temporary = tempfile.TemporaryDirectory(prefix="rules-go bootstrap ")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.action = self.root / "action cwd"
        self.action.mkdir()
        (self.action / "keep sentinel").write_text("keep")
        self.shared = self.root / "shared cache"
        self.shared.mkdir()
        (self.shared / "keep sentinel").write_text("keep")
        dependency = self.root / "dependency tree"
        self.wrapper = dependency / "go/private/rules/binary_wrapper.sh"
        self.wrapper.parent.mkdir(parents=True)
        self.wrapper.write_bytes(FIXTURE.read_bytes())
        # With no project patch, execute the pinned baseline for functional RED.
        if PATCH.exists():
            result = subprocess.run(["patch", "--batch", "--forward", "-p1", "-i", str(PATCH)],
                                    cwd=dependency, capture_output=True, text=True, timeout=EXEC_TIMEOUT)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        tools = self.root / "inert tools"
        tools.mkdir()
        recorder = tools / "recorder.py"
        recorder.write_text(RECORDER)
        self.calls = self.root / "calls.jsonl"
        self.env = os.environ.copy()
        self.env.update(FIXTURE_PYTHON=sys.executable, FIXTURE_RECORDER=str(recorder),
                        FIXTURE_CALLS=str(self.calls), FIXTURE_ROOT=str(self.root),
                        GOCACHE=str(self.shared), GOPATH=str(self.root / "shared gopath"),
                        LD_FLAGS="-X main.fixture=retained")
        self.original_home = self.env.get("HOME")
        for name in ["go", "mktemp", "mkdir", "rm"]:
            if name != "go":
                real = shutil.which(name)
                self.assertIsNotNone(real, name)
                self.env["FIXTURE_REAL_" + name.upper()] = real
            shim = tools / ("fake go" if name == "go" else name)
            shim.write_text('#!/bin/sh\nexec "$FIXTURE_PYTHON" "$FIXTURE_RECORDER" ' + name + ' "$@"\n')
            shim.chmod(0o700)
        self.env["GO_BINARY"] = str(tools / "fake go")
        self.env["PATH"] = str(tools) + os.pathsep + self.env.get("PATH", "/usr/bin:/bin")
        self.command = ["bash", str(self.wrapper), "out pack", "out binary", "source one.go", "source-two.go"]

    def rows(self):
        return [json.loads(x) for x in self.calls.read_text().splitlines()] if self.calls.exists() else []

    def run_wrapper(self, mode="success", linked_cwd=False):
        self.env["FIXTURE_MODE"] = mode
        cwd = self.action
        if linked_cwd:
            cwd = self.root / "action alias"
            cwd.symlink_to(self.action, target_is_directory=True)
        return subprocess.run(self.command, cwd=cwd, env=self.env, capture_output=True,
                              text=True, timeout=EXEC_TIMEOUT)

    def assert_no_clean(self):
        self.assertFalse(any(x["tool"] == "go" and x["argv"][0] != "build" for x in self.rows()),
                         "bootstrap invoked forbidden Go cache cleanup instead of private scratch cleanup")

    def assert_sentinels(self):
        self.assertEqual((self.shared / "keep sentinel").read_text(), "keep")
        self.assertEqual((self.action / "keep sentinel").read_text(), "keep")

    def assert_no_private_scratch(self):
        self.assertEqual(list(self.action.glob(".rules-go-bootstrap.*")), [])

    def test_success_preserves_arguments_home_and_private_cache_lifecycle(self):
        result = self.run_wrapper(linked_cwd=True)
        self.assert_no_clean()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = [x for x in self.rows() if x["tool"] == "go"]
        self.assertEqual(len(calls), 2)
        flags = '-buildid="" -X main.fixture=retained'
        self.assertEqual(calls[0]["argv"], ["build", "-trimpath", "-ldflags=" + flags, "-o", "out pack", "cmd/pack"])
        self.assertEqual(calls[1]["argv"], ["build", "-trimpath", "-ldflags=" + flags, "-o", "out binary", "source one.go", "source-two.go"])
        self.assertEqual(calls[0]["env"], calls[1]["env"])
        for call in calls:
            self.assertEqual(call["env"]["HOME"], self.original_home)
            self.assertEqual(Path(call["cwd"]), self.action)
            parents = []
            for key in ["GOCACHE", "GOPATH", "GOTMPDIR", "TMPDIR"]:
                path = Path(call["env"][key])
                self.assertTrue(path.is_absolute(), key)
                self.assertEqual(path.parent.parent, self.action, key)
                self.assertTrue(path.parent.name.startswith(".rules-go-bootstrap."), key)
                parents.append(path.parent)
            self.assertEqual(len(set(parents)), 1)
        self.assertEqual((self.action / "out pack").read_text(), "inert-output-1")
        self.assertEqual((self.action / "out binary").read_text(), "inert-output-2")
        self.assert_no_private_scratch()
        self.assert_sentinels()

    def test_first_build_failure_does_not_run_second_build(self):
        result = self.run_wrapper("fail-first")
        self.assert_no_clean()
        self.assertEqual(result.returncode, 37, result.stderr)
        self.assertEqual(len([x for x in self.rows() if x["tool"] == "go"]), 1)
        self.assert_no_private_scratch()
        self.assert_sentinels()

    def test_second_build_failure_preserves_first_output(self):
        result = self.run_wrapper("fail-second")
        self.assert_no_clean()
        self.assertEqual(result.returncode, 37, result.stderr)
        self.assertEqual(len([x for x in self.rows() if x["tool"] == "go"]), 2)
        self.assertEqual((self.action / "out pack").read_text(), "inert-output-1")
        self.assert_no_private_scratch()
        self.assert_sentinels()

    def test_creation_failures_do_not_run_builds(self):
        # Each unittest instance owns a separate directory; no active fixture is shared.
        result = self.run_wrapper("fail-mktemp")
        self.assertEqual(result.returncode, 71, result.stderr)
        self.assertEqual([x for x in self.rows() if x["tool"] == "go"], [])
        self.assert_no_private_scratch()
        self.assert_sentinels()

    def test_mkdir_failure_cleans_allocated_private_root(self):
        result = self.run_wrapper("fail-mkdir")
        self.assertEqual(result.returncode, 72, result.stderr)
        self.assertEqual([x for x in self.rows() if x["tool"] == "go"], [])
        self.assert_no_private_scratch()
        self.assert_sentinels()

    def test_cleanup_failure_is_visible_and_leak_is_local(self):
        result = self.run_wrapper("fail-rm")
        self.assert_no_clean()
        self.assertEqual(result.returncode, 73, result.stderr)
        self.assertEqual(len(list(self.action.glob(".rules-go-bootstrap.*"))), 1)
        self.assert_sentinels()

    def test_build_failure_survives_cleanup_failure(self):
        result = self.run_wrapper("fail-first-and-rm")
        self.assert_no_clean()
        self.assertEqual(result.returncode, 37, result.stderr)
        self.assertEqual(len(list(self.action.glob(".rules-go-bootstrap.*"))), 1)
        self.assert_sentinels()

    def test_interruption_has_bounded_cleanup(self):
        self.env["FIXTURE_MODE"] = "interrupt"
        proc = subprocess.Popen(self.command, cwd=self.action, env=self.env, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True, start_new_session=True)
        try:
            with selectors.DefaultSelector() as selector:
                selector.register(proc.stdout, selectors.EVENT_READ)
                self.assertTrue(selector.select(EXEC_TIMEOUT), "inert Go child never reached its signal barrier")
            self.assertEqual(proc.stdout.readline().strip(), "INERT_READY")
            os.killpg(proc.pid, signal.SIGTERM)
            stdout, stderr = proc.communicate(timeout=EXEC_TIMEOUT)
            self.assertEqual(proc.returncode, 143, stderr)
            self.assert_no_clean()
            self.assert_no_private_scratch()
            self.assert_sentinels()
        finally:
            if proc.returncode is None:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.communicate(timeout=EXEC_TIMEOUT)

    def test_module_pin_and_patch_export_are_wired(self):
        module = (REPO / "MODULE.bazel").read_text()
        self.assertIn('bazel_dep(name = "rules_go", version = "0.63.0")', module)
        blocks = re.findall(r"single_version_override\((.*?)\)", module, re.S)
        relevant = [x for x in blocks if re.search(r'module_name\s*=\s*"rules_go"', x)]
        self.assertEqual(len(relevant), 1, "rules_go needs one exact private-scratch module patch")
        self.assertIn('"//third_party/patches:rules_go_private_bootstrap_scratch.patch"', relevant[0])
        self.assertRegex(relevant[0], r"patch_strip\s*=\s*1")
        self.assertIn('"rules_go_private_bootstrap_scratch.patch"', (REPO / "third_party/patches/BUILD.bazel").read_text())

if __name__ == "__main__":
    unittest.main()
