import os
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

import runner_policy


class MacSetupArchitectureTests(unittest.TestCase):
    def test_shared_setup_accepts_both_standard_mac_architectures(self) -> None:
        action_path = Path(__file__).resolve().parents[2] / "actions/setup-gascity-macos/action.yml"
        action = action_path.read_text()
        verify = action.split("    - name: Verify macOS\n", 1)[1].split("\n    - uses:", 1)[0]
        command = textwrap.dedent(verify.split("      run: |\n", 1)[1])
        for system, architecture, expected in (
            ("Darwin", "arm64", 0), ("Darwin", "x86_64", 0),
            ("Darwin", "riscv64", 1), ("Linux", "x86_64", 1),
        ):
            with self.subTest(system=system, architecture=architecture), tempfile.TemporaryDirectory() as temporary:
                uname = Path(temporary) / "uname"
                uname.write_text(
                    '#!/bin/sh\ncase "$1" in\n'
                    ' -m) printf "%s\\n" "$FIXTURE_ARCH";;\n'
                    ' *) printf "%s\\n" "$FIXTURE_SYSTEM";;\nesac\n'
                )
                uname.chmod(0o755)
                env = dict(os.environ, PATH=temporary + ":/usr/bin:/bin",
                           FIXTURE_ARCH=architecture, FIXTURE_SYSTEM=system)
                result = subprocess.run(["bash", "-e", "-c", command], env=env,
                                        text=True, capture_output=True, timeout=5)
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)


class RunnerPolicyTests(unittest.TestCase):
    def test_load_allowlist_ignores_comments_and_case_normalizes(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "allowlist.txt"
            path.write_text(
                "julianknutsen\n"
                "  Csells  # maintainer\n"
                "\n"
                "# comment\n",
                encoding="utf-8",
            )

            self.assertEqual(runner_policy.load_allowlist(path), {"julianknutsen", "csells"})

    def test_pull_request_from_allowlisted_author_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "Quad341",
            {"quad341"},
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("allowlist", reason)
        self.assertEqual(runners["runner_32vcpu"], "blacksmith-32vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-12vcpu-macos-15")
        self.assertEqual(runners["runner_macos_quality"], "blacksmith-12vcpu-macos-15")

    def test_push_uses_github_even_for_allowlisted_author(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "push",
            "julianknutsen",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("approved pull requests", reason)
        self.assertEqual(runners["runner_32vcpu"], "ubuntu-latest")
        self.assertEqual(runners["runner_macos_quality"], "macos-15-intel")

    def test_forced_workflow_call_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "workflow_call",
            "",
            set(),
            force_blacksmith=True,
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("forced", reason)
        self.assertEqual(runners["runner_16vcpu"], "blacksmith-16vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-12vcpu-macos-15")
        self.assertEqual(runners["runner_macos_quality"], "blacksmith-12vcpu-macos-15")

    def test_unlisted_pull_request_author_uses_github(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "external-contributor",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("not on the Blacksmith allowlist", reason)
        self.assertEqual(runners["runner_macos"], "macos-15")
        self.assertEqual(runners["runner_macos_quality"], "macos-15-intel")


if __name__ == "__main__":
    unittest.main()
