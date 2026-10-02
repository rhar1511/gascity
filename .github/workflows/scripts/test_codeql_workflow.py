import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]


class CodeQLWorkflowTests(unittest.TestCase):
    def test_go_analysis_adds_local_suppression_suite_to_default_queries(self) -> None:
        workflow = (ROOT / ".github/workflows/codeql.yml").read_text()
        self.assertIn(
            "matrix.language == 'go' && '+./.github/codeql/go-alert-suppression.qls'",
            workflow,
        )

    def test_local_suite_selects_only_the_official_go_suppression_query(self) -> None:
        suite = (ROOT / ".github/codeql/go-alert-suppression.qls").read_text()
        self.assertEqual(
            suite,
            "- queries: .\n"
            "  from: codeql/go-queries\n"
            "- include:\n"
            "    query filename: AlertSuppression.ql\n",
        )


if __name__ == "__main__":
    unittest.main()
