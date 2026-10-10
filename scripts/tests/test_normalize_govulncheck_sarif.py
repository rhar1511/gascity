import copy
import importlib.util
import pathlib
import unittest

SCRIPT = pathlib.Path(__file__).parents[1] / 'normalize-govulncheck-sarif.py'
spec = importlib.util.spec_from_file_location('sarif_normalizer', SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class NormalizeSarifTest(unittest.TestCase):
    def test_only_identical_stacks_are_removed(self):
        first = {'message': {'text': 'caller'}, 'frames': [{'location': {'id': 1}}]}
        different = {'message': {'text': 'other caller'}, 'frames': [{'location': {'id': 2}}]}
        document = {'version': '2.1.0', 'runs': [{'tool': {'driver': {'name': 'govulncheck'}}, 'results': [
            {'ruleId': 'GO-test', 'level': 'error', 'message': {'text': 'reachable'},
             'stacks': [first, copy.deepcopy(first), different], 'properties': {'untouched': True}},
            {'ruleId': 'GO-test', 'level': 'note', 'message': {'text': 'required'}},
        ]}]}
        expected = copy.deepcopy(document)
        expected['runs'][0]['results'][0]['stacks'] = [first, different]
        self.assertEqual(module.normalize(document), 1)
        self.assertEqual(document, expected)
        self.assertEqual(module.normalize(document), 0)

    def test_key_order_does_not_make_a_distinct_stack(self):
        document = {'version': '2.1.0', 'runs': [{'results': [{'stacks': [
            {'message': {'text': 'same'}, 'frames': []},
            {'frames': [], 'message': {'text': 'same'}},
        ]}]}]}
        self.assertEqual(module.normalize(document), 1)

    def test_malformed_documents_are_rejected(self):
        for document in [{'version': '2.0.0', 'runs': []}, {'version': '2.1.0', 'runs': {}},
                         {'version': '2.1.0', 'runs': [{'results': [{'stacks': {}}]}]}]:
            with self.subTest(document=document), self.assertRaises(ValueError):
                module.normalize(document)


if __name__ == '__main__':
    unittest.main()
