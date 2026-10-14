"""Shell-free candidate controls; every subprocess and Git boundary is mocked."""
import copy
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from test_gascity_worker_launch import worker_launch as launch


class ProtectedPrepareTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'repo'
        self.root.mkdir()
        self.pool = Path(self.temp.name) / 'pool'
        self.path = self.pool / 'types' / 'task'
        self.path.mkdir(parents=True)
        self.env = {'GC_RIG_ROOT': str(self.root), 'GC_DIR': str(self.path),
                    'GC_RIG': 'types', 'GC_SESSION_ID': 'session',
                    'GC_TRIGGER_WORK_BEAD_ID': 'task',
                    'GC_TRIGGER_WORK_STORE_REF': 'rig:types',
                    'GC_TEMPLATE': 'types/worker',
                    'GC_ISOLATION_WORKTREES': str(self.pool)}
        self.meta = {'gc.work_dir': str(self.path), 'gc.work_branch': 'codex/task',
                     'gc.worktree_repo': str(self.root),
                     'gc.worktree_root': str(self.path.parent),
                     'gc.worktree_base_ref': 'refs/heads/main',
                     'gc.worktree_base_sha': 'a' * 40,
                     'gc.worktree_creator': 'gc-worktree',
                     'gc.worktree_owner': 'types/worker',
                     'gc.worktree_generation': '1', 'gc.worktree_lifecycle': 'active',
                     'gc.root_store_ref': 'rig:types'}
        self.work = {'id': 'task', 'status': 'open', 'metadata': self.meta}
        self.session = {'id': 'session', 'status': 'open', 'issue_type': 'session',
                        'metadata': {'template': 'types/worker',
                                     'gc.trigger_bead_id': 'task',
                                     'gc.trigger_bead_store_ref': 'rig:types',
                                     'work_dir': str(self.path)}}
        self.calls = []
        self.claim = {'action': 'work', 'bead_id': 'task'}
        self.report = {'ok': True, 'created': False, 'branch_created': False,
                       'path': str(self.path), 'provenance': {'attempt_id': 'original'}}
        self.patch('verify_fresh_base', return_value='a' * 40)
        self.patch('run_gc', side_effect=self.gc)
        guard = mock.patch.object(launch.iso, 'guard', return_value=None)
        guard.start(); self.addCleanup(guard.stop)
        git = mock.patch.object(launch.iso, 'git', side_effect=AssertionError('unexpected Git call'))
        git.start(); self.addCleanup(git.stop)

    def patch(self, name, **kwargs):
        p = mock.patch.object(launch, name, **kwargs)
        value = p.start(); self.addCleanup(p.stop)
        return value

    def gc(self, env, *args):
        self.calls.append(args)
        if args[:2] == ('beads', 'show'):
            row = self.session if args[2] == 'session' else self.work
            return json.dumps({'bead': copy.deepcopy(row), '_cache_age_s': 0})
        if args[:2] == ('worktree', 'verify') or args[:2] == ('worktree', 'ensure'):
            return json.dumps(self.report)
        if args == ('hook', '--claim', '--json'):
            return json.dumps(self.claim)
        raise AssertionError('raw claim or metadata write attempted: ' + repr(args))

    def no_claim(self):
        self.assertFalse(any(c[0] == 'hook' for c in self.calls))

    def test_native_verification_precedes_only_protected_claim(self):
        self.assertEqual(launch.prepare(self.env), self.path)
        effects = [c[:2] for c in self.calls if c[0] != 'beads']
        self.assertEqual(effects, [('worktree', 'verify'), ('worktree', 'ensure'),
                                   ('worktree', 'verify'), ('hook', '--claim')])
        self.assertFalse(any(c[0] == 'bd' for c in self.calls))

    def test_legacy_without_native_metadata_refuses_before_claim(self):
        del self.meta['gc.worktree_generation']
        with self.assertRaisesRegex(launch.iso.IsolationError, 'workspace binding'):
            launch.prepare(self.env)
        self.no_claim()

    def test_closed_session_refuses_before_workspace_or_claim(self):
        self.session['status'] = 'closed'
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)
        self.no_claim()
        self.assertFalse(any(c[0] == 'worktree' for c in self.calls))

    def test_authoritative_session_workdir_mismatch_refuses(self):
        self.session['metadata']['work_dir'] = str(self.root)
        with self.assertRaisesRegex(launch.iso.IsolationError, 'session workspace'):
            launch.prepare(self.env)
        self.no_claim()

    def test_wrong_store_or_trigger_refuses(self):
        for key in ('gc.trigger_bead_id', 'gc.trigger_bead_store_ref', 'template'):
            with self.subTest(key=key):
                previous = self.session['metadata'][key]
                self.session['metadata'][key] = 'wrong'
                with self.assertRaises(launch.iso.IsolationError):
                    launch.prepare(self.env)
                self.session['metadata'][key] = previous
                self.no_claim()

    def test_missing_or_conflicting_trigger_store_refuses(self):
        for env in ({k: v for k, v in self.env.items() if k != 'GC_TRIGGER_WORK_STORE_REF'},
                    {**self.env, 'GC_TRIGGER_WORK_BEAD_STORE_REF': 'rig:ai'}):
            with self.subTest(env=env), self.assertRaises(launch.iso.IsolationError):
                launch.prepare(env)
        self.no_claim()

    def test_no_standby_adoption(self):
        del self.env['GC_TRIGGER_WORK_BEAD_ID']
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)
        self.no_claim()

    def test_native_provenance_refusal_is_not_retrofitted(self):
        original = self.gc
        def refuse(env, *args):
            if args[:2] == ('worktree', 'verify'):
                raise launch.iso.IsolationError('native provenance missing')
            return original(env, *args)
        self.patch('run_gc', side_effect=refuse)
        with self.assertRaisesRegex(launch.iso.IsolationError, 'provenance missing'):
            launch.prepare(self.env)
        self.no_claim()
        self.assertFalse(any(c[:2] == ('worktree', 'ensure') for c in self.calls))

    def test_existing_native_resume_key_must_match_session(self):
        self.env['GC_DECLARED_NATIVE_THREAD'] = 'original-key'
        self.session['metadata']['session_key'] = 'replacement-key'
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)
        self.no_claim()

    def test_wrong_protected_claim_never_returns_workspace(self):
        self.claim['bead_id'] = 'another-task'
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)

    def test_failed_native_report_refuses(self):
        self.report['ok'] = False
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)
        self.no_claim()

    def test_creation_during_reuse_refuses(self):
        self.report['created'] = True
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)
        self.no_claim()

    def test_cross_rig_workspace_evidence_refuses(self):
        self.meta['gc.worktree_root'] = str(self.pool / 'ai')
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)
        self.no_claim()

    def test_native_attempt_changes_refuse(self):
        original = self.gc
        def changed(env, *args):
            if args[:2] == ('worktree', 'ensure'):
                self.report['provenance']['attempt_id'] = 'replacement'
            return original(env, *args)
        self.patch('run_gc', side_effect=changed)
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)
        self.no_claim()

    def test_session_rebind_between_workspace_and_claim_refuses(self):
        original = self.gc
        def rebind(env, *args):
            result = original(env, *args)
            if args[:2] == ('worktree', 'ensure'):
                self.session['metadata']['gc.trigger_bead_id'] = 'other-task'
            return result
        self.patch('run_gc', side_effect=rebind)
        with self.assertRaises(launch.iso.IsolationError):
            launch.prepare(self.env)
        self.no_claim()


if __name__ == '__main__':
    unittest.main()
