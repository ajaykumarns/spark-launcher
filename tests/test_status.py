import contextlib
import io
import json
import unittest
from unittest.mock import patch

from spark_launcher.cli import run
from spark_launcher.status import format_status


class StatusTests(unittest.TestCase):
    def setUp(self):
        self.data = {
            'nodes': {'spark01': {'memory_bytes': {'MemAvailable': 16 * 1024**3,
                                                   'MemTotal': 128 * 1024**3},
                                  'assets': [{'path': 'large-inventory-should-not-print'}]}},
            'comfyui': {'spark01': {'reachable': True, 'phase': 'executing', 'pending': 2}},
            'models': {
                'qwen': {'kind': 'llm', 'loaded': True, 'state': 'ready',
                         'nodes': ['spark01'], 'active_requests': 3},
                'parakeet': {'kind': 'audio', 'state': 'running', 'nodes': ['spark02'],
                             'active_requests': 0, 'keep_loaded': True},
                'whisper': {'kind': 'audio', 'state': 'stopped', 'nodes': []},
                'checkpoint': {'kind': 'comfy_model', 'state': 'stopped', 'nodes': []}},
            'admission': {'waiting': [{'workload': 'glm'}]}, 'activity_known': True}

    def test_no_arguments_show_help_without_config_network_or_prompt(self):
        stream = io.StringIO()
        with patch('spark_launcher.cli.load_config', side_effect=AssertionError('No config read')), \
             patch('builtins.input', side_effect=AssertionError('No prompt')), contextlib.redirect_stdout(stream):
            self.assertEqual(run([]), 0)
        self.assertIn('usage: sparky', stream.getvalue())
        self.assertIn('status', stream.getvalue())

    def test_status_table_explains_activity_without_inventory_or_raw_json(self):
        with patch('spark_launcher.cli.load_config', return_value={}), \
             patch('spark_launcher.cli.remote_status', return_value=self.data):
            stream = io.StringIO()
            with contextlib.redirect_stdout(stream):
                self.assertEqual(run(['status']), 0)
        text = stream.getvalue()
        for value in ('16.0 GiB', 'executing (2 queued)', 'qwen', 'ready', 'Waiting: glm',
                      'Kept loaded: parakeet.', 'whisper'):
            self.assertIn(value, text)
        self.assertNotIn('large-inventory-should-not-print', text)
        self.assertNotIn('checkpoint', text)
        self.assertNotIn('"memory_bytes"', text)
        self.assertLess(len(text.splitlines()), 35)

    def test_explicit_json_keeps_full_status_for_scripts(self):
        with patch('spark_launcher.cli.load_config', return_value={}), \
             patch('spark_launcher.cli.remote_status', return_value=self.data):
            stream = io.StringIO()
            with contextlib.redirect_stdout(stream):
                self.assertEqual(run(['status', '--json']), 0)
        self.assertEqual(json.loads(stream.getvalue()), self.data)

    def test_unknown_activity_and_memory_are_not_reported_as_zero(self):
        self.data['nodes']['spark01']['memory_bytes'] = {}
        self.data['activity_known'] = False
        self.data['admission']['config_transition'] = True
        text = format_status(self.data)
        self.assertIn('unknown', text)
        self.assertIn('Active LLM call counts are unavailable.', text)
        self.assertIn('New work is paused', text)


if __name__ == '__main__':
    unittest.main()
