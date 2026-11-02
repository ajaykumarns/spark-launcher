# Sparky menu bar (macOS)

A native macOS status icon that opens a compact 360 pt popover panel. It shows
the Sparks' live model state and lets you explicitly load a model. All business
logic, state, and CLI work stay in Go: the app runs the existing `sparky` CLI in
the background with `--json`. A small AppKit bridge only draws the view. There is
no new server, endpoint, web view, or controller.

## Build

Requires macOS with cgo (the GUI toolchain), plus the existing CLI build.

```sh
# CLI + native app + dist/Sparky.app (bundles a standalone CLI)
make menubar

# Rebuild only the .app from existing dist/ artifacts
make package-menubar

# Headless check: the menubar command still builds without cgo
make menubar-headless
```

`make menubar` produces:

- `dist/sparky` — the headless CLI (`CGO_ENABLED=0`), unchanged in behavior.
- `dist/sparky-menubar` — the native app (`CGO_ENABLED=1`, AppKit).
- `dist/Sparky.app` — `Contents/MacOS/sparky-menubar` plus the bundled
  `Contents/Resources/sparky` CLI, `LSUIElement` set (no Dock icon), and a
  `manifest.txt` of build hashes.

`make menubar-headless` builds the cgo-free stub (`dist/darwin-arm64/` and
`dist/linux-arm64/sparky-menubar-stub`), so the CLI and install builds never
depend on AppKit.

Packaging on its own:

```sh
sh tools/package-menubar.sh [OUT_APP] [--cli PATH]
```

`OUT_APP` defaults to `dist/Sparky.app`; `--cli PATH` must be absolute. The
script has no installer side effects: it does not install, launch, register a
login item, or change preferences.

## Install

Copy the finished bundle, then launch it:

```sh
cp -R dist/Sparky.app ~/Applications/     # or /Applications/

open ~/Applications/Sparky.app
```

That is the whole install: a `.app` with a bundled CLI. Nothing runs an
installer, registers a login item, or changes preferences. Start at login stays
off until you turn it on in the settings menu.

## Run

```sh
open dist/Sparky.app

# or run the app binary directly (for development)
dist/sparky-menubar [--cli PATH] [--help] [--version]
```

Flags:

- `--cli PATH` — use this absolute Sparky CLI instead of the bundled one.
  A relative or unusable path fails closed rather than silently falling back.
- `--help` / `-h`, `--version` — print help or the app version and exit.

CLI resolution order (never the GUI shell `PATH`):

1. An explicit `--cli PATH` when given: it wins and fails closed if invalid.
2. Otherwise, the bundled CLI at `Sparky.app/Contents/Resources/sparky`, then a
   saved override, then the existing client config's `binaries.sparky` entry
   (`$SPARKY_CONFIG` or `~/.config/sparky/client.json`).
3. Conventional install locations: `~/.local/bin/sparky`, `/usr/local/bin/sparky`,
   `/opt/homebrew/bin/sparky`.

Only one app instance runs at a time (lock at `$SPARKY_STATE_DIR/menubar.lock`,
default `~/.local/state/sparky/menubar.lock`).

## The panel

Click the status icon to open a compact card:

- Header: "Sparky" with the current state word: Ready, Loading, Idle,
  Stopping, Unreachable, Failed, or Unknown.
- Language model: a strong model name and a friendly placement line such as
  "Ready on Spark 1 + Spark 2", using the actual loaded model, not a configured
  default. Extra loaded LLMs get one short line each. When the Sparks cannot be
  reached, the last known model name is shown and marked unavailable.
- Device memory: aligned rows per Spark showing in-use / total GiB (total minus
  available). Unknown or offline readings show "Unavailable", never zero.
- Active requests, with unknown kept distinct from zero. With more than one
  loaded model the label says "Active requests · N models".
- Actions: "Load model…" opens the model picker; "Refresh status" reads once
  now. Loading is offered only when the current state is connected; without a
  CLI, Refresh is disabled too, and the panel keeps diagnostics and Quit.
- Footer: last update or last contact age, or load progress text.

The icon itself can show a short title for the loaded model (off by default).

Status refreshes in the background at a low rate, never overlaps reads, backs
off after failures, and keeps showing cached data when the Sparks are
unreachable. Opening the panel also requests a refresh.

## Loading a model

"Load model…" replaces the body with a picker listing the configured concrete
models from `sparky --json models` (the synthetic `spark-auto` route is not
offered). "Back" returns to the overview. One click then runs
`sparky --json load <canonical-id>` in the background.

- Only one load runs at a time; duplicate clicks are refused.
- The panel shows loading state and progress text, never a made-up percentage,
  and only says ready when the controller reports the model ready.
- No model loads on startup or from polling. A load only starts from a click.
- Closing or hiding the panel does not cancel a load; quitting does not unload
  or cancel an active remote operation. Quit waits for an in-flight CLI load.
- A failed load appears as a short line ("Last load failed" or "Load readiness
  not confirmed"), with full detail in Copy diagnostics.

## Settings

A gear in the footer opens a small settings menu, so the panel stays focused:

- "Start at login" (off by default; toggles a per-user LaunchAgent at
  `~/Library/LaunchAgents/local.sparky.menubar.plist`, no admin rights, does not
  start or change any Spark service).
- "Show title" for the short status icon title.
- "Copy diagnostics": version, CLI path or error, preferences, status and
  staleness, last error, last load error, refresh result, per-node memory, and
  loaded models. No secrets.
- "Quit Sparky".

Preferences live in `$SPARKY_STATE_DIR/menubar-prefs.json` (default
`~/.local/state/sparky/menubar-prefs.json`).

## Errors

User-facing text is short and plain: CLI error envelopes, exit codes, malformed
output, and authentication or network failures are summarized without raw
stderr dumps. Stale data stays visible with an Unreachable state. If no CLI can
be found, the panel still opens and shows the problem, with Load and Refresh
disabled; the settings menu keeps Copy diagnostics and Quit usable.

## Signing and notarization

When `codesign` is present, the package script signs the nested bundled CLI, the
app binary, and the bundle with a local **ad-hoc** signature (`codesign --sign -`)
and verifies the staged bundle. That is a local build marker, not notarization.

The bundle is **not notarized**. It has no Developer ID signature and no Apple
notarization ticket, so another Mac may still show a Gatekeeper warning. Without
`codesign`, the script leaves the bundle unsigned and says so.

## Headless behavior

The CLI and every `CGO_ENABLED=0` Darwin/Linux build are unchanged. Without cgo
or on another platform, `cmd/sparky-menubar` builds a small stub that prints a
"needs macOS with cgo" message and exits 2, so headless CI stays green.
