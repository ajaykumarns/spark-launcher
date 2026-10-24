# Sparky

One Python command controls the two Sparks and launches Claude Code, Hermes, Codex, or OpenCode.

```sh
sparky
sparky claude --model nemotron
sparky hermes --model glm
sparky codex --model deepseek --project ~/dev/project
sparky opencode --model qwen
```

Running `sparky` without arguments shows help. Normal commands use bordered tables, clear section headings, and separate detail cards. Running states are green. Stopped states are dim. Warnings are yellow and errors are red. State labels remain visible without color. Colors are automatic in a terminal and disabled in redirected output. Use `--color always` to force them, or `--color never` to disable them. `NO_COLOR` is respected in automatic mode. Very narrow terminals use cards. Every public data command accepts `--json` for scripts. You can put it before or after the command. Nested configuration commands also accept it at the end.

```sh
sparky status                       # What is running and how much RAM is available
sparky models                       # Names and configured context limits; no model load
sparky models qwen                  # Full model ID, aliases, limits, and configured devices
sparky models --installed           # Include audio, ComfyUI, and image checkpoints
sparky devices                      # RAM, disk, GPU allocation, and containers
sparky plan load glm                # Explain required swaps without changing services
sparky config show                  # Central configuration as a table
sparky config show qwen             # Per-placement model settings
sparky config validate              # A validation result, without a file dump
sparky logs --limit 5               # Local harness launch history
sparky doctor                       # Check every installed harness and the gateway
```

A configured route is not proof that a model is loaded. Use `status` for actual running placement. `models --installed` describes installed image weights as **on demand**. Their memory residency is not inferred from files on disk.

Long model operations print progress to stderr. They finish active work before a device changes. A load prints the idle models it needs to unload. ComfyUI submission returns a job ID; it does not claim that execution has finished. Pressing Ctrl+C stops the local wait. Remote work may still continue. Check `sparky status` before another operation.

For launch previews, use `--dry-run`. Use `--prepare` to write harness settings without starting it. Both print a short human summary. Add `--json` only if a script needs the full launch plan. To pass a harness's own options, put them after `--`.

```sh
sparky claude --model qwen --dry-run
sparky codex --model nemotron --prepare
sparky codex --model qwen -- exec --json 'Explain this project'
```

JSON stdout contains exactly one result object. Progress and banners do not enter it. Script errors use JSON on stderr and a nonzero exit status. Exit 1 means an operation failed. Exit 2 means command syntax is invalid. Exit 130 means the local wait was interrupted. `doctor` and `devices` can return a partial result with exit 1. Native harnesses keep their own output and exit status. Sparky's `--json` applies to harness previews; a harness's native JSON flag goes after `--`.

```sh
sparky status --json
sparky --json plan load glm
sparky config show qwen --json
sparky logs --limit 5 --json
```

`ui --json` returns the UI URL without opening a browser. Logs can be read without gateway access or a client file. Bad history files are skipped and reported. `--offline` always labels cached model data. SSH operations give a sign-in link when Tailscale needs an identity check. They do not print private SSH diagnostics.

## Central configuration

The authoritative model file lives on Spark 1:

```
/home/spark-user/spark-cluster/config/models.json
```

It lists installed LLMs, Parakeet, Whisper, both ComfyUI engines, and installed ComfyUI checkpoints. Whisper stays disabled. Parakeet has `keep_loaded: true` and appears in `policy.protected_models`.

Each LLM has a `run_on` value: `spark01`, `spark02`, or `both`. Its `settings` contain the supported placements. Each placement specifies its nodes, systemd units, model context, backend address, startup memory check, and numeric runtime parameters. A two-device placement runs one distributed model. It does not create two independent copies. The prepared Qwen two-device recipe needs a separate runtime load test before its performance can be claimed.

Use [cluster.example.json](cluster.example.json) as the current site example. Paths and device addresses must match the site. Edit the file on Spark 1. The node files on Spark 2 are generated projections. They are not another model authority.

```sh
sparky config show
sparky config validate
sparky plan load qwen --on both
sparky config placement qwen both
sparky load qwen
```

A placement change applies on the next load. Use a plan first to see which idle models must leave memory. Resource plans never stop services.

## Resource control

The controller runs as `sparky-controller.service` on Spark 1. The existing llama-swap manager still loads models, drains active LLM requests, and applies the three-hour idle timeout. Sparky derives its compatibility matrix from the central model placements. Nemotron on Spark 1 and Qwen on Spark 2 can run together. The two-device models require both nodes.

The controller tracks node reservations for LLM requests and ComfyUI jobs. Conflicting work waits. Active LLM requests and active image jobs finish before a conflicting model is unloaded. Unknown activity prevents unloading. Idle ComfyUI caches can be released. Protected Parakeet containers stay loaded. These are software admission checks. They do not create a hardware GPU memory cap.

A ComfyUI custom node reserves the device for the full execution of each job. This also covers jobs submitted through the existing ComfyUI UI. Existing services, workflows, assets, and image outputs stay in place. The controller uses no kernel, driver, or global memory changes.

```sh
sparky status
sparky devices
sparky models --installed
sparky plan load glm
sparky load glm
sparky unload glm
sparky run comfyui workflow-api.json --on spark02
```

`workflow-api.json` must be a ComfyUI API workflow. The installed checkpoints include image models. No dedicated video checkpoint was found during inventory.

## Harness configuration

`GET /sparky/catalog` exposes the central names, aliases, context limits, output budgets, modalities, and selected devices through the existing keyless inference ingress. The client fetches this catalog before each launch. It has no local model settings to override it. `spark-auto` keeps its existing gateway alias and uses the common context budget.

The local file `~/.config/sparky/client.json` stores only gateway, SSH, client preferences, and executable paths. See [config.example.json](config.example.json). `SPARKY_CONFIG` selects another client file. `SPARKY_STATE_DIR` selects another local state directory. Use `--offline` explicitly to accept cached discovery data.

| Harness | Integration |
|---|---|
| Claude Code | Bifrost `/anthropic`, model mappings, context environment, request timeout |
| Hermes | Existing `providers.spark-gateway`, live catalog and contexts, native provider and model flags |
| Codex | Bifrost `/v1/responses`, generated model catalog, explicit context and compact threshold |
| OpenCode | Per-process `OPENCODE_CONFIG_CONTENT`, `/v1`, live model metadata |

The launcher preserves unrelated providers, credentials, MCP settings, permissions, and history. Hermes requires a narrow update to its existing Spark provider block. The launcher takes a private backup and preserves unrelated YAML values and comments. Other adapters use process settings or generated files. No adapter enables permission bypasses.

Some clients require a nonempty API token field. Their fixed placeholder is not a gateway secret. Inference remains keyless through Tailscale. Administrative SSH keeps its existing checks. Sparky is not an access boundary for a process that already runs as the administrator.

```sh
sparky claude --model qwen --dry-run
sparky codex --model nemotron --prepare
sparky sync
sparky doctor
sparky claude --model qwen -- --continue
sparky opencode --model glm -- run 'Explain this project'
```

Claude Code's context environment applies at launch. Start a new process to select a model with a different context window. Each harness retains its own summary policy. The Codex compact threshold defaults to 95 percent of context.

## Logs and access

Bifrost keeps the existing SQLite LLM call logs. Sparky adds SQLite resource decision records on Spark 1 at `controller/events.sqlite3`. Local launch records contain model, context, project, timestamps, and exit status. They do not contain prompts, tokens, copied environments, or credentials. Private configuration backups stay outside the repository.

The controller binds loopback for gateway and administrator operations. Its direct-link listener accepts only scoped ComfyUI job operations from the configured node. Public ingress adds only the read-only catalog route. Administrator endpoints and credentials remain separate.

## Install and verify

Python 3.11 or later is required. The client uses Rich for terminal tables and colors. The node archive includes the terminal renderer, so it needs no new system packages. The client uses `ruamel.yaml` to preserve Hermes comments. The controller uses the existing PyYAML installation to validate native manager configuration.

```sh
uv tool install .
sparky --help
make test
uv run make node-helper
```

Copy `dist/sparky.pyz` to `~/.local/bin/sparky` on both Sparks. Stage the cluster configuration on Spark 1. The explicit node installer creates the controller service and ComfyUI guards. It waits for image queues to drain before it restarts each ComfyUI service. It preserves running LLM and Parakeet containers. It keeps private backups of changed server files.

```sh
sparky node install --dir /path/to/staged-config
```

The staged folder must contain `models.json`. The first install creates the central file. A later install preserves an existing central file. The `node` command is the private machine protocol. Normal users do not need it. Its output stays JSON for the manager and SSH transport.

`sparky` is the sole command. Old `spark`, `spark-models`, and `opencode-spark` commands are removed. There are no compatibility wrappers.

The review and checks are documented in [docs/usability-review.md](docs/usability-review.md).

Focused verification tools cover resource planning, node reservations, guarded ComfyUI execution, numeric recipe rendering, native harness parsers, and synthetic harness requests. Synthetic tests do not prove model quality. Live checks must distinguish a prepared recipe from a loaded and verified model.
