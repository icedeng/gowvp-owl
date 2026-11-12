import copy
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('gateway_release', Path(__file__).with_name('release.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def plan(self):
        targets = {role: {'ssh': 'root@gateway.example.com', 'port': 22, 'directory': '/data/owl', 'key': __file__, 'version': 'test', 'current_sha256': 'a'*64, 'candidate_sha256': 'b'*64, 'backup_paths': ['/data/owl/compose.yml'], 'compose_file': '/data/owl/compose.yml', 'service': role, 'project': 'owl', 'healthcheck_required': True} for role in ('a', 'b', 'zlm')}
        targets['a'].update(database_service='postgres', database_name='owl', database_user='owl')
        targets['b'].update(unit='owl-gateway.service', binary_rel='owl-gateway', config_path='/data/owl/config.toml', state_path='/data/owl/state', backup_paths=['/data/owl/config.toml', '/data/owl/state'])
        with tempfile.NamedTemporaryFile(delete=False) as f:
            f.write(b'\x7fELFtest'); f.flush(); self.addCleanup(Path(f.name).unlink)
            targets['b'].update(artifact=f.name, file_sha256=release.digest(f.name), candidate_sha256=release.digest(f.name))
        current = {role: 'a'*64 for role in targets}
        return {'release_id': 'test-1', 'api_base': 'http://127.0.0.1/api', 'targets': targets, 'steps': ['b'], 'gateways': ['b1'], 'schema_backward_compatible': True, 'compatible_sets': [current, {**current, 'b': targets['b']['candidate_sha256']}]}

    def test_validation_rejects_missing_backup_and_incompatible_combination(self):
        plan = self.plan(); release.validate(plan)
        bad = copy.deepcopy(plan); bad['targets']['b']['backup_paths'] = []
        with self.assertRaises(ValueError): release.validate(bad)
        bad = copy.deepcopy(plan); bad['compatible_sets'].pop()
        with self.assertRaises(ValueError): release.validate(bad)
        bad = copy.deepcopy(plan); bad['targets']['b']['directory'] = '/data/owl;reboot'
        with self.assertRaises(ValueError): release.validate(bad)
        bad = copy.deepcopy(plan); bad['schema_backward_compatible'] = False
        with self.assertRaises(ValueError): release.validate(bad)

    def test_failed_upgrade_rolls_back_and_report_failure_does_not_undo_success(self):
        plan = self.plan()
        with patch.dict(os.environ, {'OWL_RELEASE_TOKEN': 'test'}): runner = release.Release(plan)
        calls = []
        def remote(role, action, expected=None):
            calls.append((role, action))
            if action == 'stage': return {'stage': '/data/owl/packages/test/artifact'}
            if action == 'upgrade': raise ValueError('simulate failed start')
            return {'previous': 'old'}
        runner.remote = remote
        runner.gateway = lambda gid: {'admin_state': 'enabled'}
        runner.state = lambda gid, state: calls.append(('gateway', state))
        runner.verify = lambda state: calls.append(('verify', tuple(state)))
        runner.report = lambda *args: calls.append(('report', args[1]))
        with patch.object(release.subprocess, 'run'):
            with self.assertRaises(ValueError): runner.apply()
        self.assertIn(('b', 'rollback'), calls)
        self.assertLess(calls.index(('b', 'rollback')), calls.index(('gateway', 'enabled')))
        self.assertIn(('report', 'rolled_back'), calls)
        self.assertIn(('a', 'release'), calls)
        runner.api = lambda *args: (_ for _ in ()).throw(ValueError('old A report endpoint missing'))
        runner.report = release.Release.report.__get__(runner)
        runner.gateway = lambda gid: {'revision': '1'}
        runner.report('b', 'succeeded', 'old')
        self.assertIn('pending_report', runner.evidence[-1])

    def test_remote_script_compiles(self):
        compile(release.REMOTE, 'remote', 'exec')


if __name__ == '__main__': unittest.main()
