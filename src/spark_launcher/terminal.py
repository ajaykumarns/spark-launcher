"""Shared terminal styling. Machine output never enters this module's renderer."""
from __future__ import annotations

import os
import shutil
import sys
from contextvars import ContextVar
from io import StringIO

from rich import box
from rich.console import Console
from rich.panel import Panel
from rich.table import Table
from rich.text import Text

COLOR = ContextVar('sparky_color', default='auto')


def clean(value):
    return ''.join(c if c.isprintable() else ' ' for c in str(value))


def configure_from_args(argv):
    """Help is handled during parsing, so read the color option first."""
    options = argv[:argv.index('--')] if '--' in argv else argv
    selected = 'auto'
    for index, value in enumerate(options):
        if value.startswith('--color='):
            selected = value.split('=', 1)[1]
        elif value == '--color' and index + 1 < len(options):
            selected = options[index + 1]
    COLOR.set(selected if selected in ('auto', 'always', 'never') else 'auto')


def use_color(stream=None):
    mode = COLOR.get()
    if mode != 'auto':
        return mode == 'always'
    if 'NO_COLOR' in os.environ or os.environ.get('TERM') == 'dumb':
        return False
    if os.environ.get('FORCE_COLOR', '0') != '0':
        return True
    return bool(getattr(stream or sys.stdout, 'isatty', lambda: False)())


def terminal_box():
    encoding = getattr(sys.stdout, 'encoding', None) or 'utf-8'
    return box.ASCII if os.environ.get('TERM') == 'dumb' or 'utf' not in encoding.lower() else box.ROUNDED


def render(item, width=None, stream=None):
    buffer = StringIO()
    enabled = use_color(stream)
    console = Console(file=buffer, force_terminal=enabled, color_system='standard' if enabled else None,
                      no_color=not enabled, width=width or shutil.get_terminal_size((100, 24)).columns,
                      legacy_windows=False, highlight=False, markup=False)
    console.print(item, end='')
    return buffer.getvalue().rstrip('\n')


def state_style(value):
    value = str(value).lower()
    if any(word in value for word in ('unavailable', 'failed', 'error', 'unknown')):
        return 'bold bright_red'
    if any(word in value for word in ('waiting', 'paused', 'prepared', 'not verified', 'queued')) and not value.startswith('idle'):
        return 'yellow'
    if value in ('stopped', 'disabled', '-', 'on demand', 'not finished'):
        return 'dim'
    if value in ('ready', 'running', 'ok', 'enabled', 'yes') or value.startswith('executing'):
        return 'bold bright_green'
    if value.startswith('idle'):
        return 'bright_cyan'
    return ''


def table(headers, rows, width=None, title=None):
    headers = tuple(clean(value) for value in headers)
    rows = [tuple(clean(value) for value in row) for row in rows]
    available = width or shutil.get_terminal_size((100, 24)).columns
    if available < 60:
        # Keep every value on small screens. A dense six-column grid cannot fit.
        cards = []
        for row in rows:
            cards.append(key_values(title or 'Details', list(zip(headers, row)), width=available))
        return '\n'.join(cards)
    grid = Table(title=Text(clean(title), style='bold bright_cyan') if title else None,
                 title_justify='left', box=terminal_box(), border_style='bright_black',
                 header_style='bold bright_cyan', padding=(0, 1), show_lines=headers[0] in ('STEP', 'TIME (LOCAL)'),
                 expand=False, safe_box=False, collapse_padding=False)
    for index, header in enumerate(headers):
        numeric = any(word in header for word in ('CONTEXT', 'OUTPUT', 'CALLS', 'SLOTS', 'SEQUENCES', 'RAM', 'BUDGET'))
        grid.add_column(header, justify='right' if numeric else 'left', overflow='fold',
                        style='bold' if index == 0 else '', min_width=3,
                        no_wrap=index == 0 and 'MODEL' in header)
    for row in rows:
        grid.add_row(*(Text(value, style=state_style(value)) for value in row))
    return render(grid, width=available)


def heading(title):
    return render(Text(clean(title), style='bold bright_cyan'))


def note(message, tone='dim'):
    return render(Text(clean(message), style={'green': 'bright_green', 'yellow': 'bright_yellow'}.get(tone, tone)))


def key_values(title, rows, tone='cyan', width=None):
    grid = Table.grid(padding=(0, 2), expand=False)
    grid.add_column(style='dim', no_wrap=False)
    grid.add_column(overflow='fold')
    for label, value in rows:
        grid.add_row(Text(clean(label) + ':'), Text(clean(value), style=state_style(value)))
    return render(Panel(grid, title=Text(clean(title), style='bold ' + tone), title_align='left',
                        border_style=tone, box=terminal_box(), padding=(0, 1), expand=False,
                        safe_box=False), width=width)


def notice(title, message, tone='green', stream=None):
    body = Text()
    for index, line in enumerate(str(message).splitlines()):
        if index:
            body.append('\n')
        body.append(clean(line))
    return render(Panel(body, title=Text(clean(title), style='bold ' + tone), title_align='left',
                        border_style=tone, box=terminal_box(), padding=(0, 1), expand=False,
                        safe_box=False), stream=stream)


def help_view(text):
    body = Text(text.rstrip())
    body.highlight_regex(r'(?m)^[A-Za-z][^\n]*:$', 'bold bright_cyan')
    body.highlight_regex(r'--[a-z][a-z-]*', 'green')
    body.highlight_regex(r'\bsparky\b', 'bold bright_cyan')
    return render(Panel(body, title=Text('sparky', style='bold bright_cyan'), title_align='left',
                        border_style='bright_black', box=terminal_box(), padding=(0, 1),
                        safe_box=False)) + '\n'
