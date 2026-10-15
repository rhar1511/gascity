"""Source-owned native provenance must never be relabelled to its execution step."""
import copy
import json
import unittest

import test_protected_prepare as support

launch = support.launch


class SourceWorkspaceTests(unittest.TestCase):
    setUp = support.ProtectedPrepareTests.setUp
    patch = support.ProtectedPrepareTests.patch
    no_claim = support.ProtectedPrepareTests.no_claim

    def gc(self, env, *args):
        if args[:3] == ('beads', 'show', 'source'):
            self.calls.append(args)
            return json.dumps({'bead': copy.deepcopy(self.source), '_cache_age_s': 0})
        return support.ProtectedPrepareTests.gc(self, env, *args)

    def source_fixture(self):
        self.lineage = {'version': 1, 'state': 'attached', 'scope': 'city:pilot/rig:types',
                        'contract': 'signed-contract-digest', 'token': 'reservation-token',
                        'route': 'types/worker', 'workflow': 'mol-do-work',
                        'merge_strategy': 'mr', 'workflow_id': 'workflow',
                        'source_id': 'source', 'source_store_ref': 'rig:types',
                        'workflow_store_ref': 'rig:types', 'admission_receipt': 'admission-proof'}
        self.source = {'id': 'source', 'status': 'open', 'metadata': copy.deepcopy(self.meta)}
        self.source['metadata'].update({'gc.routed_to': 'types/worker', 'molecule_id': 'workflow',
                                        'merge_strategy': 'mr'})
        # The execution step deliberately has no workspace metadata and no drain member.
        self.work['metadata'] = {'gc.root_store_ref': 'rig:types'}
        self.write_lineage()

    def write_lineage(self, step=None, source=None):
        key = 'gc.lifecycle.materialization.v1'
        self.work['metadata'][key] = json.dumps(step or self.lineage)
        self.source['metadata'][key] = json.dumps(source or self.lineage)

    def test_native_owner_is_source_but_protected_claim_is_step(self):
        self.source_fixture()
        self.assertEqual(launch.prepare(self.env), self.path)
        native = [c for c in self.calls if c[0] == 'worktree']
        self.assertEqual(len(native), 3)
        self.assertTrue(all(c[c.index('--bead') + 1] == 'source' for c in native))
        self.assertEqual(self.calls[-1], ('hook', '--claim', '--json'))
        self.assertEqual(self.claim['bead_id'], 'task')

    def test_lineage_pending_step_retains_original_source_owner(self):
        self.source_fixture()
        step = {**self.lineage, 'state': 'lineage_pending'}
        del step['workflow_id']
        self.write_lineage(step=step)
        self.assertEqual(launch.prepare(self.env), self.path)

    def test_conflicting_lineage_refuses_before_native_and_claim(self):
        self.source_fixture()
        for key in ('scope', 'contract', 'token', 'admission_receipt', 'route',
                    'source_store_ref', 'workflow_store_ref', 'workflow', 'merge_strategy'):
            with self.subTest(key=key):
                self.write_lineage(step={**self.lineage, key: 'wrong'})
                with self.assertRaises(launch.iso.IsolationError):
                    launch.prepare(self.env)
                self.no_claim()
                self.assertFalse(any(c[0] == 'worktree' for c in self.calls))

    def test_closed_source_is_not_reopened(self):
        self.source_fixture()
        self.source['status'] = 'closed'
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)
        self.no_claim()

    def test_ordinary_step_without_drain_metadata_maps_source(self):
        self.source_fixture()
        self.assertNotIn('gc.drain_member_id', self.work['metadata'])
        self.assertEqual(launch.prepare(self.env), self.path)

    def test_copied_step_workspace_metadata_cannot_relabel_source(self):
        self.source_fixture()
        self.work['metadata'].update(self.meta)
        self.work['metadata']['gc.worktree_generation'] = 'copied-step-generation'
        self.assertEqual(launch.prepare(self.env), self.path)
        for args in (c for c in self.calls if c[0] == 'worktree'):
            self.assertEqual(args[args.index('--bead') + 1], 'source')
            self.assertEqual(args[args.index('--generation') + 1], '1')

    def test_missing_source_workspace_is_not_replaced_with_step_metadata(self):
        self.source_fixture()
        self.work['metadata'].update(self.meta)
        del self.source['metadata']['gc.worktree_generation']
        with self.assertRaisesRegex(launch.iso.IsolationError, 'workspace binding'):
            launch.prepare(self.env)
        self.no_claim()


    def test_source_without_optional_graph_store_marker_reaches_exact_step_claim(self):
        self.source_fixture()
        del self.source['metadata']['gc.root_store_ref']
        self.assertEqual(self.work['metadata']['gc.root_store_ref'], 'rig:types')
        self.assertEqual(launch.prepare(self.env), self.path)
        native = [call for call in self.calls if call[0] == 'worktree']
        self.assertEqual(len(native), 3)
        self.assertTrue(all(call[call.index('--bead') + 1] == 'source' for call in native))
        self.assertTrue(all(call[call.index('--store-ref') + 1] == 'rig:types' for call in native))
        self.assertEqual(self.calls[-1], ('hook', '--claim', '--json'))
        self.assertNotIn('gc.root_store_ref', self.source['metadata'])


    def test_explicit_foreign_or_empty_source_graph_store_refuses(self):
        for marker in ('rig:ai', '', None):
            with self.subTest(marker=marker):
                self.source_fixture()
                self.source['metadata']['gc.root_store_ref'] = marker
                with self.assertRaisesRegex(launch.iso.IsolationError, 'workspace store'):
                    launch.prepare(self.env)
                self.no_claim()
                self.assertFalse(any(call[0] == 'worktree' for call in self.calls))

    def test_execution_step_still_requires_its_exact_graph_store(self):
        self.source_fixture()
        del self.source['metadata']['gc.root_store_ref']
        del self.work['metadata']['gc.root_store_ref']
        with self.assertRaisesRegex(launch.iso.IsolationError, 'source linkage crosses'):
            launch.prepare(self.env)
        self.no_claim()
        self.assertFalse(any(call[0] == 'worktree' for call in self.calls))


if __name__ == '__main__':
    unittest.main()
