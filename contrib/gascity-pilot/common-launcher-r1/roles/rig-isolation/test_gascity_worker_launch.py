import importlib.util
import json
import tempfile
import unittest
from unittest import mock
from pathlib import Path

MODULE_PATH = Path(__file__).with_name("gascity-worker-launch.py")
SPEC = importlib.util.spec_from_file_location("gascity_worker_launch", MODULE_PATH)
worker_launch = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(worker_launch)


class ProviderCommandTests(unittest.TestCase):
    def test_codex_replaces_never_with_automatic_approval_review(self):
        command = [
            "/opt/codex",
            "--no-alt-screen",
            "-a",
            "never",
            "resume",
            "session-key",
        ]

        actual = worker_launch.provider_command(command)

        self.assertEqual(
            actual,
            [
                "/opt/codex",
                "--approve-for-me",
                "--no-alt-screen",
                "resume",
                "session-key",
            ],
        )

    def test_codex_removes_long_form_never(self):
        command = [
            "/opt/codex",
            "--ask-for-approval=never",
            "--model",
            "gpt-6-luna",
        ]

        actual = worker_launch.provider_command(command)

        self.assertEqual(
            actual,
            [
                "/opt/codex",
                "--approve-for-me",
                "--model",
                "gpt-6-luna",
            ],
        )

    def test_non_codex_provider_is_unchanged(self):
        command = ["/opt/opencode", "serve"]

        self.assertEqual(worker_launch.provider_command(command), command)


class BaseFreshnessTests(unittest.TestCase):
    def test_matching_authority_allows_launch(self):
        oid = "a" * 40
        with mock.patch.object(worker_launch.iso, "git", side_effect=[oid, f"{oid}\trefs/heads/main"]):
            self.assertEqual(worker_launch.verify_fresh_base(Path("/rig"), {}), oid)

    def test_stale_local_main_refuses_before_claim(self):
        with mock.patch.object(
            worker_launch.iso,
            "git",
            side_effect=["a" * 40, f"{'b' * 40}\trefs/heads/main"],
        ):
            with self.assertRaisesRegex(worker_launch.iso.IsolationError, "local main is stale"):
                worker_launch.verify_fresh_base(Path("/rig"), {})

    def test_missing_authority_ref_refuses(self):
        with mock.patch.object(worker_launch.iso, "git", side_effect=["a" * 40, ""]):
            with self.assertRaisesRegex(worker_launch.iso.IsolationError, "returned no commit"):
                worker_launch.verify_fresh_base(Path("/rig"), {})


class DeclaredLaunchContextTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.city = Path(self.temp.name)
        self.launch = self.city / "launch"
        self.launch.mkdir()
        self.env = {
            "GC_CITY_PATH": str(self.city), "GC_SESSION_ID": "test-session",
            "GC_AGENT": "inktree/test-worker", "GC_TEMPLATE": "inktree/worker",
            "GC_DIR": str(self.launch),
        }
        self.inputs = {
            "GC_RIG": "inktree", "GC_RIG_ROOT": str(self.city / "rig"),
            "GC_BIN": "/test/gc", "GC_ISOLATION_WORKTREES": str(self.city / "pool"),
            "GC_TRIGGER_WORK_BEAD_ID": "inktree-task",
            "GC_TRIGGER_WORK_BEAD_STORE_REF": "rig:inktree",
        }
        self.record = {
            "schema": 1, "session": "test-session", "agent": self.env["GC_AGENT"],
            "template": "inktree/worker", "launch_dir": str(self.launch), "env": self.inputs,
        }
        self.declaration = self.city / ".gc/runtime/worker-launch-context/test-session.json"
        self.declaration.parent.mkdir(parents=True)
        self.declaration.parent.chmod(0o700)

    def write(self):
        self.declaration.write_text(json.dumps(self.record))
        self.declaration.chmod(0o600)

    def restore(self, env=None):
        return worker_launch.declared_launch_context(env or self.env, self.city)

    def test_missing_declaration_does_not_invent_rig_context(self):
        self.assertEqual(self.restore(), self.env)
        self.assertNotIn("GC_RIG_ROOT", self.restore())

    def test_exact_declaration_restores_only_missing_inputs(self):
        self.write()
        self.assertEqual(self.restore(), {**self.env, **self.inputs,
                                         "GC_TRIGGER_WORK_STORE_REF": "rig:inktree"})
        self.assertNotIn("GC_RIG", self.env)

    def test_conflicting_canonical_trigger_store_refused(self):
        self.write()
        with self.assertRaises(worker_launch.iso.IsolationError):
            self.restore({**self.env, "GC_TRIGGER_WORK_STORE_REF": "rig:ai"})

    def test_other_session_cannot_borrow_declaration(self):
        self.write()
        other = {**self.env, "GC_SESSION_ID": "other-session"}
        self.assertEqual(self.restore(other), other)

    def test_wrong_identity_or_launch_directory_refused(self):
        self.write()
        for key, value in (
            ("GC_AGENT", "inktree/unrelated"), ("GC_TEMPLATE", "frontend/worker"),
            ("GC_CITY_PATH", "/wrong-city"), ("GC_DIR", str(self.city)),
        ):
            with self.subTest(key=key), self.assertRaises(worker_launch.iso.IsolationError):
                self.restore({**self.env, key: value})

    def test_conflicting_trigger_refused(self):
        self.write()
        with self.assertRaisesRegex(worker_launch.iso.IsolationError, "conflicts"):
            self.restore({**self.env, "GC_TRIGGER_WORK_BEAD_ID": "inktree-unrelated"})

    def test_incomplete_or_extra_inputs_refused(self):
        for change in (lambda: self.inputs.pop("GC_RIG_ROOT"),
                       lambda: self.inputs.update({"API_TOKEN": "unexpected"})):
            with self.subTest(change=change):
                original = dict(self.inputs)
                change()
                self.write()
                with self.assertRaisesRegex(worker_launch.iso.IsolationError, "inputs"):
                    self.restore()
                self.inputs.clear()
                self.inputs.update(original)

    def test_mismatched_record_session_or_store_refused(self):
        self.record["session"] = "other-session"
        self.write()
        with self.assertRaisesRegex(worker_launch.iso.IsolationError, "session mismatch"):
            self.restore()
        self.record["session"] = "test-session"
        self.inputs["GC_TRIGGER_WORK_BEAD_STORE_REF"] = "rig:frontend"
        self.write()
        with self.assertRaisesRegex(worker_launch.iso.IsolationError, "rig store mismatch"):
            self.restore()

    def test_writable_or_symlink_declaration_refused(self):
        self.write()
        self.declaration.chmod(0o666)
        with self.assertRaisesRegex(worker_launch.iso.IsolationError, "permissions"):
            self.restore()
        self.declaration.unlink()
        target = self.city / "target.json"
        target.write_text(json.dumps(self.record))
        self.declaration.symlink_to(target)
        with self.assertRaisesRegex(worker_launch.iso.IsolationError, "symlink"):
            self.restore()

    def test_dangling_symlink_is_not_treated_as_absent(self):
        self.declaration.symlink_to(self.city / "missing.json")
        with self.assertRaisesRegex(worker_launch.iso.IsolationError, "symlink"):
            self.restore({**self.env, **self.inputs})

    def test_writable_or_linked_declaration_directory_refused(self):
        self.write()
        self.declaration.parent.chmod(0o777)
        with self.assertRaisesRegex(worker_launch.iso.IsolationError, "permissions"):
            self.restore()
        self.declaration.parent.chmod(0o755)
        directory = self.declaration.parent
        destination = directory.with_name("actual-context")
        directory.rename(destination)
        directory.symlink_to(destination, target_is_directory=True)
        with self.assertRaisesRegex(worker_launch.iso.IsolationError, "symlinks"):
            self.restore()

    def test_exact_owned_directory_can_restore_later_resume_inputs(self):
        self.write()
        owned = Path(self.inputs["GC_ISOLATION_WORKTREES"]) / "inktree/inktree-task/test-session"
        owned.mkdir(parents=True)
        env = {**self.env, "GC_DIR": str(owned)}
        self.assertEqual(self.restore(env), {**env, **self.inputs,
                                            "GC_TRIGGER_WORK_STORE_REF": "rig:inktree"})

    def test_declared_native_thread_is_preserved(self):
        native = "01a11040-123c-7ad2-b7d6-967b17e2f60e"
        self.record["native_thread"] = native
        self.write()
        restored = self.restore()
        self.assertEqual(restored["GC_DECLARED_NATIVE_THREAD"], native)

    def test_present_but_invalid_native_binding_refused(self):
        for value in ("", None, False, 0, "not-a-uuid"):
            self.record["native_thread"] = value
            self.write()
            with self.subTest(value=value), self.assertRaises(worker_launch.iso.IsolationError):
                self.restore()


class DeclaredResumeTests(unittest.TestCase):
    def test_first_launch_without_native_binding_is_unchanged(self):
        worker_launch.verify_declared_resume({}, ["/opt/codex", "--model", "configured"])

    def test_exact_resume_is_allowed(self):
        worker_launch.verify_declared_resume(
            {"GC_DECLARED_NATIVE_THREAD": "original"},
            ["/opt/codex", "--no-daemon", "resume", "original"],
        )

    def test_fresh_or_other_resume_or_fallback_provider_is_refused(self):
        for command in (
            ["/opt/codex"], ["/opt/codex", "resume", "replacement"],
            ["/opt/codex", "resume", "--last"], ["/opt/opencode", "resume", "original"],
            ["/opt/codex", "--profile", "resume", "original"],
            ["/opt/codex", "--model", "resume", "original"],
            ["/opt/codex", "--config", "resume", "original"],
            ["/opt/codex", "exec", "resume", "original"],
        ):
            with self.subTest(command=command), self.assertRaises(worker_launch.iso.IsolationError):
                worker_launch.verify_declared_resume({"GC_DECLARED_NATIVE_THREAD": "original"}, command)

    def test_configured_worker_global_options_preserve_exact_resume(self):
        worker_launch.verify_declared_resume(
            {"GC_DECLARED_NATIVE_THREAD": "original"},
            ["/opt/codex", "--no-daemon", "--no-alt-screen", "-a", "never",
             "-c", 'default_permissions="gascity-pilot"', "-c", "features.network_proxy=false",
             "--model", "gpt-6-luna", "-c", 'model_reasoning_effort="max"', "resume", "original"],
        )


if __name__ == "__main__":
    unittest.main()
