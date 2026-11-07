import copy
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import shutil

import codex_reconfigure as configure
from codex_reconfigure import migrate_bindings


class ProviderBindingMigrationTests(unittest.TestCase):
    def test_rename_preserves_history_ids_and_session_options(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            row = {'history': [{'role': 'assistant', 'content': 'preserve me'}],
                   'agent_session_id': 'native-id', 'past_agent_session_ids': ['older'],
                   'active_provider': 'Seed', 'provider_bound': True,
                   'model_override': 'chosen', 'service_tier': 'fast'}
            data = {'sessions': {'s1': row, 's2': dict(row, active_provider='', agent_session_id='legacy')},
                    'active_session': 's2', 'user_sessions': {'topic': ['s1', 's2']}}
            before = copy.deepcopy(data)
            (root / 'codex_ws_test.json').write_text(json.dumps(data))
            (root / 'claude.json').write_text(json.dumps(data))
            changed = migrate_bindings(root, lambda _: 'csj')
            after = json.loads((root / 'codex_ws_test.json').read_text())
            self.assertEqual(after['sessions']['s1']['active_provider'], 'maso')
            self.assertEqual(after['sessions']['s2']['active_provider'], 'AIDP')
            for sid, old in before['sessions'].items():
                for key in old:
                    if key not in ('active_provider', 'provider_bound'):
                        self.assertEqual(after['sessions'][sid][key], old[key])
            self.assertEqual(after['active_session'], before['active_session'])
            self.assertEqual(after['user_sessions'], before['user_sessions'])
            self.assertEqual(json.loads((root / 'claude.json').read_text()), before)
            self.assertEqual(len(changed['codex_ws_test.json']), 2)
            self.assertEqual(migrate_bindings(root, lambda _: 'csj'), {})

    def test_rollback_restores_regular_file_replaced_by_symlink_and_bindings(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            sessions = root / 'sessions'
            sessions.mkdir()
            data = {'sessions': {'s1': {'active_provider': 'Seed', 'provider_bound': True, 'history': ['saved']}}}
            (sessions / 'codex.json').write_text(json.dumps(data))
            shutil.copytree(sessions, root / 'session-files.before')
            target = root / 'old-launcher'
            target.write_text('original launcher')
            candidate = root / 'candidate'
            candidate.write_text('new launcher')
            installed = root / 'new-launcher'
            retired = root / 'retired'
            retired.mkdir()
            (retired / 'config').write_text('original configuration')
            archive = root / 'archive'
            archive.mkdir()
            manifest = root / 'deployment.json'
            manifest.write_text('{}')
            def record(path, **fields):
                current = json.loads(path.read_text())
                current.update(fields)
                path.write_text(json.dumps(current))
            def require(condition, message):
                if not condition:
                    raise RuntimeError(message)
            worker = SimpleNamespace(SESSIONS=sessions, base=SimpleNamespace(START='original-start'),
                                     verify_sessions=lambda *a: None, check=lambda *a: {},
                                     require=require, record=record)
            plan = {'archive': str(archive), 'files': [
                {'target': str(target), 'before': configure.fingerprint(target), 'link': str(installed)},
                {'target': str(installed), 'before': None, 'source': str(candidate), 'sha256': configure.fingerprint(candidate)}],
                'retire_paths': [str(retired)], 'old_processes': [], 'new_services': [], 'start_script': 'new-start'}
            configure.patch_worker(worker, manifest, plan, SimpleNamespace(saved_provider=lambda *a: None))
            with patch.object(configure.subprocess, 'run'), patch.object(configure.time, 'sleep'):
                worker.install_config({})
                self.assertTrue(target.is_symlink())
                self.assertFalse(retired.exists())
                self.assertEqual(json.loads((sessions / 'codex.json').read_text())['sessions']['s1']['active_provider'], 'maso')
                worker.install_config({}, rollback=True)
            self.assertFalse(target.is_symlink())
            self.assertEqual(target.read_text(), 'original launcher')
            self.assertFalse(installed.exists())
            self.assertEqual((retired / 'config').read_text(), 'original configuration')
            self.assertEqual(json.loads((sessions / 'codex.json').read_text()), data)


if __name__ == '__main__':
    unittest.main()
