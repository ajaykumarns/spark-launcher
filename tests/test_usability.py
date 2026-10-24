"""Public command contracts: readable for people; one JSON document for scripts."""
import contextlib
import io
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from spark_launcher.catalog import SparkError, resolve, validate_config
from spark_launcher.cli import main, read_workflow, run
from spark_launcher.control import run_remote_json
from spark_launcher.presentation import format_devices, format_logs, table
from test_resources import configuration, make_plan, FakeRuntime


class UsabilityTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.env = patch.dict(os.environ, {'SPARKY_STATE_DIR': str(self.root / 'state'),
                                          'SPARKY_CONFIG': str(self.root / 'client.json')})
        self.env.start()
        self.config = validate_config({'version': 1, 'gateway': 'https://spark.example:8443',
            'admin_url': 'https://spark.example', 'admin_ssh_host': 'head', 'remote_root': '/srv/cluster',
            'binaries': {h: '/usr/bin/true' for h in ('claude', 'hermes', 'codex', 'opencode')}})
        self.cluster = configuration(self.root)
        self.state = FakeRuntime(self.cluster).data
        self.state['nodes'] = {'spark01': {'memory_bytes': {'MemAvailable': 16 * 1024**3}}}
        self.state['admission'] = {'waiting': []}
        self.state['models']['parakeet'] = {'kind': 'audio', 'loaded': True, 'nodes': ['spark02'],
                                          'state': 'running', 'desired': 'spark02', 'keep_loaded': True}
        for row in self.state['models'].values():
            row.setdefault('desired', 'spark02')
        self.catalog = {'gateway': self.config['gateway'], 'fetched_at': '2026-10-24T00:00:00+00:00',
            'models': [{'id': 'garuda/qwen', 'name': 'Qwen', 'aliases': ['qwen'],
                        'context_tokens': 262144, 'output_tokens': 16384, 'nodes': ['spark02']}]}
        self.workflow = self.root / 'workflow.json'
        self.workflow.write_text(json.dumps({'1': {'class_type': 'SparkyProbe', 'inputs': {}}}))
        self.document = {'path': '/srv/cluster/config/models.json', 'config': self.cluster}

    def tearDown(self):
        self.env.stop()
        self.temp.cleanup()

    def rpc(self, config, operation, payload):
        if operation == 'config':
            if payload.get('model'):
                return {**self.document, 'model': 'qwen', 'config': self.cluster['models']['qwen']}
            return self.document
        if operation == 'validate':
            return {'valid': True, 'path': self.document['path'], 'model_count': len(self.cluster['models'])}
        if operation == 'placement':
            return {'workload': payload['model'], 'run_on': payload['on']}
        if operation == 'plan':
            return make_plan(self.cluster, payload['model'], self.state, payload['action'], payload.get('on'))
        if operation == 'run':
            return {'node': 'spark02', 'prompt_id': 'test-job'}
        raise AssertionError(operation)

    def invoke(self, args):
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch('spark_launcher.cli.load_config', return_value=self.config), \
             patch('spark_launcher.cli.fetch_catalog', return_value=self.catalog), \
             patch('spark_launcher.cli.remote_status', return_value=self.state), \
             patch('spark_launcher.cli.remote_rpc', side_effect=self.rpc), \
             patch('spark_launcher.cli.remote_result', side_effect=lambda c, a, m=None: {'status': 'ready', 'workload': m}), \
             patch('spark_launcher.cli.devices', return_value={'ok': True, 'devices': {'spark01': {'hostname': 'head'}}}), \
             patch('spark_launcher.cli.sync_hermes', return_value=None), \
             patch('spark_launcher.cli.prepare'), patch('spark_launcher.cli.webbrowser.open', return_value=True), \
             contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = run(args)
        return code, stdout.getvalue(), stderr.getvalue()

    def test_all_public_data_commands_have_human_and_machine_output(self):
        cases = [(['models'], 'CONTEXT'), (['models', 'qwen'], 'Model ID:'),
                 (['models', '--installed'], 'INSTALLED MODEL'), (['status'], 'AVAILABLE RAM'),
                 (['devices'], 'Free disk:'), (['plan', 'load', 'deepseek'], 'No services were changed.'),
                 (['config', 'show'], 'Authoritative file'), (['config', 'show', 'qwen'], 'Selected placement:'),
                 (['config', 'validate'], 'Configuration is valid.'),
                 (['config', 'placement', 'qwen', 'both'], 'Running services were not moved.'),
                 (['run', 'comfyui', str(self.workflow)], 'Execution may wait'),
                 (['load', 'qwen'], 'Ready: qwen.'), (['unload', 'qwen'], 'Unloaded: qwen.'),
                 (['sync'], 'Hermes Spark provider is current.'), (['doctor'], 'No inference was run.'),
                 (['logs'], 'No local harness launches recorded.'), (['ui'], 'Opened gateway UI:'),
                 (['claude', '--model', 'qwen', '--dry-run'], 'Launch preview.'),
                 (['hermes', '--model', 'qwen', '--prepare'], 'No harness was started.')]
        for args, expected in cases:
            with self.subTest(args=args, mode='human'):
                code, stdout, _ = self.invoke(args)
                self.assertEqual(code, 0)
                self.assertIn(expected, stdout)
                self.assertFalse(stdout.lstrip().startswith(('{', '[')), stdout)
            with self.subTest(args=args, mode='json'):
                code, stdout, stderr = self.invoke(['--json', *args])
                self.assertEqual(code, 0)
                self.assertIsInstance(json.loads(stdout), dict)
                self.assertEqual(stderr, '')

    def test_init_has_human_and_json_results(self):
        for json_mode in (False, True):
            target = self.root / ('init-' + str(json_mode) + '.json')
            stream = io.StringIO()
            args = ['init', '--gateway', 'https://spark.example:8443'] + (['--json'] if json_mode else [])
            with patch.dict(os.environ, {'SPARKY_CONFIG': str(target)}), contextlib.redirect_stdout(stream):
                self.assertEqual(run(args), 0)
            self.assertTrue(target.exists())
            if json_mode:
                self.assertEqual(json.loads(stream.getvalue())['status'], 'created')
            else:
                self.assertIn('Client configuration created:', stream.getvalue())

    def test_real_child_json_transport_keeps_input_and_output_separate(self):
        import sys
        script = 'import json,sys; data=json.load(sys.stdin); print(json.dumps({"received": data["value"]}))'
        result = run_remote_json([sys.executable, '-c', script], payload=json.dumps({'value': 'input data'}))
        self.assertEqual(result, {'received': 'input data'})

    def test_very_narrow_table_uses_labeled_rows(self):
        text = table(('PLACEMENT', 'CONTEXT', 'REQUEST SLOTS', 'ENGINE SEQUENCES', 'GPU BUDGET'),
                     [('spark02', '262,144', '4', '4', '75%')], width=30)
        self.assertTrue(all(len(line) <= 30 for line in text.splitlines()))
        self.assertIn('PLACEMENT: spark02', text)


    def test_json_option_works_before_and_after_nested_commands(self):
        for args in (['--json', 'config', 'show'], ['config', '--json', 'show'], ['config', 'show', '--json']):
            self.assertEqual(json.loads(self.invoke(args)[1]), self.document)

    def test_local_logs_do_not_need_a_client_file_or_network(self):
        with patch('spark_launcher.cli.load_config', side_effect=AssertionError('No config read')):
            stream = io.StringIO()
            with contextlib.redirect_stdout(stream):
                self.assertEqual(run(['logs']), 0)
        self.assertIn('No local harness launches recorded', stream.getvalue())

    def test_bad_log_does_not_hide_valid_history(self):
        folder = self.root / 'state/launches'
        folder.mkdir(parents=True)
        (folder / 'bad.json').write_text('{broken')
        (folder / 'good.json').write_text(json.dumps({'started_at': '2026-10-24T00:00:00+00:00',
            'harness': 'claude', 'model': 'garuda/qwen', 'project': '/tmp/project', 'exit_code': 0}))
        code, text, _ = self.invoke(['logs'])
        self.assertEqual(code, 0)
        self.assertIn('claude', text)
        self.assertIn('Skipped 1 unreadable', text)

    def test_workflow_ui_export_error_is_actionable_and_does_not_submit(self):
        self.workflow.write_text(json.dumps({'nodes': [], 'links': []}))
        with patch('spark_launcher.cli.remote_rpc') as remote:
            with self.assertRaisesRegex(SparkError, 'Export it in API format'):
                self.invoke(['run', 'comfyui', str(self.workflow)])
        remote.assert_not_called()
        self.workflow.unlink()
        with self.assertRaisesRegex(SparkError, 'Workflow file not found'):
            read_workflow(self.workflow)

    def test_doctor_reports_all_checks_when_gateway_is_down(self):
        from spark_launcher.cli import doctor
        with patch('spark_launcher.cli.fetch_catalog', side_effect=SparkError('Gateway offline')), \
             patch('spark_launcher.cli.executable', side_effect=SparkError('Harness missing')):
            result = doctor(self.config)
        self.assertFalse(result['ok'])
        self.assertEqual(len(result['checks']), 5)

    def test_json_errors_use_stderr_and_nonzero_exit(self):
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.object(__import__('sys'), 'argv', ['sparky', '--json', 'status']), \
             patch('spark_launcher.cli.load_config', return_value=self.config), \
             patch('spark_launcher.cli.remote_status', side_effect=SparkError('Device unavailable')), \
             contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            with self.assertRaises(SystemExit) as result:
                main()
        self.assertEqual(result.exception.code, 1)
        self.assertEqual(stdout.getvalue(), '')
        self.assertEqual(json.loads(stderr.getvalue())['error'], 'Device unavailable')

    def test_native_json_argument_does_not_switch_sparky_error_format(self):
        from spark_launcher.cli import json_requested
        self.assertFalse(json_requested(['codex', '--', 'exec', '--json']))
        self.assertTrue(json_requested(['--json', 'codex', '--dry-run']))

    def test_syntax_errors_use_exit_two_with_json(self):
        stderr = io.StringIO()
        with patch.object(__import__('sys'), 'argv', ['sparky', '--json', 'logs', '--limit', '0']), \
             contextlib.redirect_stderr(stderr):
            with self.assertRaises(SystemExit) as result:
                main()
        self.assertEqual(result.exception.code, 2)
        self.assertIn('positive whole number', json.loads(stderr.getvalue())['error'])

    def test_model_aliases_ignore_case_and_offer_typo_hints(self):
        self.assertEqual(resolve(self.catalog, 'QWEN')['id'], 'garuda/qwen')
        with self.assertRaisesRegex(SparkError, 'Did you mean qwen'):
            resolve(self.catalog, 'qwe')

    def test_missing_device_keeps_other_device_readable(self):
        text = format_devices({'devices': {'spark01': {'error': 'offline'},
                                          'spark02': {'hostname': 'worker', 'memory_bytes': {'MemAvailable': 8 * 1024**3}}}})
        self.assertIn('spark01: unavailable', text)
        self.assertIn('8.0 GiB', text)
        self.assertNotIn('{', text)

    def test_table_fits_narrow_terminal_without_losing_values(self):
        text = table(('MODEL', 'CONTEXT', 'PLACEMENT'), [('a-model-with-a-very-long-name', '262,144', 'spark01, spark02')], width=48)
        self.assertTrue(all(len(line) <= 48 for line in text.splitlines()))
        self.assertIn('262,144', text)
        self.assertNotIn('...', text)

    def test_remote_error_message_survives_without_raw_stderr(self):
        from unittest.mock import MagicMock
        process = MagicMock()
        process.returncode = 1
        process.communicate.return_value = (json.dumps({'error': 'parakeet must stay loaded.'}), 'raw-private-diagnostic')
        process.__enter__.return_value = process
        with patch('spark_launcher.control.subprocess.Popen', return_value=process):
            with self.assertRaisesRegex(SparkError, '^parakeet must stay loaded.$'):
                run_remote_json(['ssh', 'host'])

    def test_identity_check_is_shown_before_long_load_timeout(self):
        from unittest.mock import MagicMock
        process = MagicMock()
        process.__enter__.return_value = process
        process.communicate.side_effect = [subprocess.TimeoutExpired(['ssh'], 5,
            stderr=b'Private text. Visit https://login.tailscale.com/a/abc123'), ('', '')]
        with patch('spark_launcher.control.subprocess.Popen', return_value=process):
            with self.assertRaisesRegex(SparkError, r'Open https://login.tailscale.com/a/abc123') as error:
                run_remote_json(['ssh'], timeout=7200)
        self.assertNotIn('Private text', str(error.exception))
        process.kill.assert_called_once()

    def test_unknown_command_offers_correct_name(self):
        from spark_launcher.cli import UsageError
        with self.assertRaisesRegex(UsageError, 'Did you mean status'):
            run(['stats'])

    def test_machine_ui_output_does_not_open_a_browser(self):
        with patch('spark_launcher.cli.webbrowser.open') as browser, \
             patch('spark_launcher.cli.load_config', return_value=self.config), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(run(['ui', '--json']), 0)
        browser.assert_not_called()


if __name__ == '__main__':
    unittest.main()
