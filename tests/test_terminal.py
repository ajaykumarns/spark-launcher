"""Check real terminal styling, redirected text, width, and JSON isolation."""
import contextlib
import io
import json
import os
import re
import unittest
from unittest.mock import patch

from spark_launcher.cli import run
from spark_launcher.status import format_status
from spark_launcher.terminal import COLOR, configure_from_args, table


class TTY(io.StringIO):
    def isatty(self):
        return True


class TerminalTests(unittest.TestCase):
    def setUp(self):
        self.token = COLOR.set('auto')
        self.env = patch.dict(os.environ, {'TERM': 'xterm-256color', 'COLUMNS': '100'}, clear=True)
        self.env.start()
        self.status = {'nodes': {'spark01': {'memory_bytes': {'MemAvailable': 16*1024**3, 'MemTotal': 128*1024**3}}},
            'comfyui': {'spark01': {'reachable': True, 'phase': 'idle', 'pending': 0}},
            'models': {'qwen': {'kind': 'llm', 'state': 'ready', 'loaded': True, 'nodes': ['spark01'], 'active_requests': 2},
                       'glm': {'kind': 'llm', 'state': 'stopped', 'nodes': []}},
            'admission': {'waiting': []}}

    def tearDown(self):
        self.env.stop()
        COLOR.reset(self.token)

    def test_interactive_terminal_gets_real_borders_and_semantic_colors(self):
        with contextlib.redirect_stdout(TTY()):
            text = format_status(self.status)
        self.assertIn('╭', text)
        self.assertIn('╰', text)
        self.assertIn('│', text)
        self.assertRegex(text, r'\x1b\[[0-9;]*92mready')
        self.assertIn('\x1b[2mstopped', text)
        self.assertIn('Spark cluster status', text)
        self.assertIn('Models and audio', text)

    def test_redirected_output_keeps_borders_without_ansi(self):
        with contextlib.redirect_stdout(io.StringIO()):
            text = format_status(self.status)
        self.assertIn('╭', text)
        self.assertNotIn('\x1b', text)

    def test_no_color_and_explicit_override(self):
        os.environ['NO_COLOR'] = ''
        with contextlib.redirect_stdout(TTY()):
            self.assertNotIn('\x1b', format_status(self.status))
            configure_from_args(['--color', 'always', 'status'])
            self.assertIn('\x1b', format_status(self.status))
            configure_from_args(['status', '--color', 'never'])
            self.assertNotIn('\x1b', format_status(self.status))

    def test_dumb_terminal_uses_ascii_borders(self):
        os.environ['TERM'] = 'dumb'
        with contextlib.redirect_stdout(TTY()):
            text = format_status(self.status)
        self.assertIn('+', text)
        self.assertNotIn('╭', text)
        self.assertNotIn('\x1b', text)

    def test_json_is_identical_even_when_color_is_forced(self):
        for options in (['--json', '--color', 'always', 'status'], ['status', '--json', '--color', 'always']):
            output = TTY()
            with patch('spark_launcher.cli.load_config', return_value={}), \
                 patch('spark_launcher.cli.remote_status', return_value=self.status), contextlib.redirect_stdout(output):
                self.assertEqual(run(options), 0)
            self.assertEqual(json.loads(output.getvalue()), self.status)
            self.assertNotIn('\x1b', output.getvalue())
            self.assertNotIn('╭', output.getvalue())

    def test_bordered_tables_fit_common_widths_without_ellipsis(self):
        rows = [('deepseek-v4.1-flash-1017', 'ready', 'spark01, spark02', '4'),
                ('qwen3.8-flash-next', 'stopped', '-', '0')]
        for width in (60, 80, 100, 120):
            configure_from_args(['--color', 'always'])
            text = table(('MODEL', 'STATE', 'DEVICE', 'CALLS'), rows, width=width)
            plain = re.sub(r'\x1b\[[0-9;]*m', '', text)
            self.assertTrue(all(len(line) <= width for line in plain.splitlines()), (width, plain))
            self.assertNotIn('…', plain)
            self.assertIn('flash-1017', plain)

    def test_help_is_styled_without_needing_configuration(self):
        output = TTY()
        with patch('spark_launcher.cli.load_config', side_effect=AssertionError('No config read')), contextlib.redirect_stdout(output):
            self.assertEqual(run([]), 0)
        self.assertIn('╭', output.getvalue())
        self.assertIn('\x1b', output.getvalue())
        self.assertIn('Examples:', output.getvalue())

