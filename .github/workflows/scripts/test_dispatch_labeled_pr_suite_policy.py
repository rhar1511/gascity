import io
import json
import os
from pathlib import Path
import textwrap
import unittest
from unittest.mock import patch


WORKFLOW = Path(__file__).resolve().parents[1] / "dispatch-labeled-pr-suite.yml"


class DispatchLabeledPRSuitePolicyTests(unittest.TestCase):
    def run_dispatch(self, *, changed_policy=False, status=204, tag_status=201,
                     missing_policy=False, truncated=False, api_error=None, tag_response=None,
                     policy_sha="c" * 40, fail_endpoint=None, **overrides):
        env = dict(LABEL_NAME="needs-review-formulas", PR_DRAFT="false",
                   PR_AUTHOR="owner", PR_ASSOCIATION="OWNER",
                   REPOSITORY="rhar1511/gascity", PR_HEAD_REPO="rhar1511/gascity",
                   BASE_REF="main", BASE_SHA="b" * 40, PR_HEAD_SHA="a" * 40,
                   PR_HEAD_REF="feature", PR_NUMBER="38", GH_TOKEN="test",
                   GITHUB_RUN_ID="123", GITHUB_RUN_ATTEMPT="1")
        env.update(overrides)
        requests = []
        self.requests = requests

        def urlopen(request, timeout):
            requests.append(request)
            if api_error is not None:
                raise api_error
            if fail_endpoint and request.full_url.endswith(fail_endpoint):
                raise OSError("API transport failed")
            response = unittest.mock.MagicMock()
            response.status = tag_status if request.full_url.endswith("/git/refs") else status
            response.__enter__.return_value = response
            if "/git/commits/" in request.full_url:
                data = {"tree": {"sha": request.full_url.rsplit("/", 1)[1]}}
            elif "/git/trees/" in request.full_url:
                is_head = request.full_url.endswith("a" * 40)
                data = {"tree": [{"path": ".github", "type": "tree",
                                  "sha": "d" * 40 if changed_policy and is_head else policy_sha}]}
                data["truncated"] = truncated
                if missing_policy:
                    data["tree"] = []
            elif request.full_url.endswith("/git/refs"):
                created = json.loads(request.data)
                data = tag_response if tag_response is not None else {
                    "ref": created["ref"], "object": {"type": "commit", "sha": created["sha"]}}
            else:
                data = {}
            response.read.return_value = json.dumps(data).encode()
            return response

        source = textwrap.dedent(WORKFLOW.read_text().split("python3 - <<'PY'\n", 1)[1]).rsplit("PY", 1)[0]
        with patch.dict(os.environ, env, clear=True), patch("pathlib.Path.read_text", return_value="allowed-user\n"), patch("urllib.request.urlopen", side_effect=urlopen), patch("sys.stdout", new_callable=io.StringIO):
            try:
                exec(compile(source, str(WORKFLOW), "exec"), {})
            except SystemExit as error:
                if error.code != 0:
                    raise
        return requests

    def test_exact_head_dispatch_for_both_labels_and_success_statuses(self):
        for label, workflow in [("needs-review-formulas", "review-formulas.yml"), ("needs-mac", "mac-regression.yml")]:
            for status in (200, 204):
                with self.subTest(label=label, status=status):
                    requests = self.run_dispatch(LABEL_NAME=label, status=status)
                    writes = [r for r in requests if r.get_method() == "POST"]
                    self.assertEqual(len(writes), 2)
                    tag = json.loads(writes[0].data)
                    dispatch = json.loads(writes[1].data)
                    self.assertEqual(tag["sha"], "a" * 40)
                    self.assertEqual("refs/tags/" + dispatch["ref"], tag["ref"])
                    self.assertEqual(dispatch["inputs"]["head_sha"], tag["sha"])
                    self.assertTrue(writes[1].full_url.endswith(workflow + "/dispatches"))
                    self.assertEqual(dispatch["inputs"]["head_repo"], "rhar1511/gascity")
                    if label == "needs-mac":
                        self.assertEqual(dispatch["inputs"]["suite"], "needs-mac")
                    self.assertEqual([r.full_url for r in requests[:4]], [
                        "https://api.github.com/repos/rhar1511/gascity/git/" + kind + "/" + sha
                        for sha in ("b" * 40, "a" * 40) for kind in ("commits", "trees")])

    def test_dispatch_ref_ignores_branch_name_and_is_unique_per_attempt(self):
        refs = []
        for attempt in ("1", "2"):
            requests = self.run_dispatch(PR_HEAD_REF="main", GITHUB_RUN_ATTEMPT=attempt)
            payload = json.loads(requests[-1].data)
            self.assertNotEqual(payload["ref"], "main")
            self.assertIn("a" * 40, payload["ref"])
            refs.append(payload["ref"])
        self.assertNotEqual(*refs)

    def test_untrusted_base_branch_prevents_dispatch(self):
        for base_ref in ("feature", "", "Main"):
            with self.subTest(base_ref=base_ref):
                requests = self.run_dispatch(BASE_REF=base_ref)
                self.assertFalse(requests, "Non-main policy must not authorize API operations")

    def test_changed_policy_fails_before_any_write(self):
        with self.assertRaisesRegex(RuntimeError, "policy"):
            self.run_dispatch(changed_policy=True)
        self.assertFalse(any(r.get_method() == "POST" for r in self.requests))

    def test_ineligible_requests_do_not_write(self):
        for overrides in [dict(PR_DRAFT="true"), dict(PR_ASSOCIATION="NONE"),
                          dict(PR_HEAD_REPO="someone/gascity"), dict(REPOSITORY="other/gascity")]:
            with self.subTest(overrides=overrides):
                self.assertFalse(any(r.get_method() == "POST" for r in self.run_dispatch(**overrides)))

    def test_policy_read_failures_prevent_writes(self):
        for options in [dict(missing_policy=True), dict(truncated=True),
                        dict(api_error=RuntimeError("API unavailable")),
                        dict(PR_HEAD_SHA="feature")]:
            with self.subTest(options=options), self.assertRaises(RuntimeError):
                self.run_dispatch(**options)
            self.assertFalse(any(r.get_method() == "POST" for r in self.requests))

    def test_invalid_policy_identity_prevents_writes(self):
        for sha in (None, "", "invalid", "c" * 39):
            with self.subTest(sha=sha), self.assertRaisesRegex(RuntimeError, "policy tree SHA"):
                self.run_dispatch(policy_sha=sha)
            self.assertFalse(any(r.get_method() == "POST" for r in self.requests))

    def test_tag_failure_prevents_dispatch(self):
        with self.assertRaisesRegex(RuntimeError, "tag creation"):
            self.run_dispatch(tag_status=422)
        self.assertFalse(any(r.full_url.endswith("/dispatches") for r in self.requests))

    def test_unexpected_tag_target_prevents_dispatch(self):
        for response in [
            {},
            {"ref": "refs/tags/wrong", "object": {"type": "commit", "sha": "a" * 40}},
            {"ref": "refs/tags/gc-ci/pr-38/" + "a" * 40 + "/123-1",
             "object": {"type": "commit", "sha": "b" * 40}},
            {"ref": "refs/tags/gc-ci/pr-38/" + "a" * 40 + "/123-1",
             "object": {"type": "tag", "sha": "a" * 40}},
        ]:
            with self.subTest(response=response), self.assertRaisesRegex(RuntimeError, "tag target"):
                self.run_dispatch(tag_response=response)
            self.assertFalse(any(r.full_url.endswith("/dispatches") for r in self.requests))

    def test_write_transport_failure_propagates_without_retry(self):
        # A lost response may follow a successful write. Retrying must not
        # duplicate a run or reuse a tag whose creation was not confirmed.
        for endpoint, expected_writes in [("/git/refs", 1), ("/dispatches", 2)]:
            with self.subTest(endpoint=endpoint), self.assertRaisesRegex(OSError, "transport failed"):
                self.run_dispatch(fail_endpoint=endpoint)
            writes = [r for r in self.requests if r.get_method() == "POST"]
            self.assertEqual(len(writes), expected_writes)
            self.assertTrue(writes[-1].full_url.endswith(endpoint))

    def test_dispatch_failure_is_reported(self):
        with self.assertRaisesRegex(RuntimeError, "dispatch status"):
            self.run_dispatch(status=202)

    def test_allowlisted_author_can_dispatch(self):
        requests = self.run_dispatch(PR_AUTHOR="Allowed-User", PR_ASSOCIATION="NONE")
        self.assertTrue(requests[-1].full_url.endswith("/dispatches"))


if __name__ == "__main__":
    unittest.main()
