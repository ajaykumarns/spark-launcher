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

`sparky status` groups running workloads by Spark. Each row shows the context limit, measured GPU allocation, and host RAM. Host RAM uses proportional resident memory (PSS), which divides shared pages among their processes. GPU memory includes weights and allocated context caches. These are observations of shared physical RAM and can overlap. Do not add the two columns.

The status probe reads NVIDIA per-process allocations, process memory summaries, exact container identities, and systemd cgroups. It includes Parakeet, ComfyUI, other containers, and other host processes. Missing measurements show `unknown`. Context comes from the running backend when it publishes a limit. A `*` marks a configured fallback. The LLM calls column is omitted. Full byte counts and sources remain in `status --json`.

LLM container identities for measurement are declared in each model's `monitoring.containers` list in the central file. This metadata does not change placement, model limits, or startup recipes. Sampling uses the existing administrator SSH and sudo access. It does not create a monitor service or change the running engines.

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

Language model idle unloading is disabled. Shared ComfyUI execution requires an explicit model allowlist and a tested memory minimum. See Resource control below.

Each LLM has a `run_on` value: `spark01`, `spark02`, or `both`. Its `settings` contain the supported placements. Each placement specifies its nodes, systemd units, model context, backend address, startup memory check, and numeric runtime parameters. A two-device placement runs one distributed model. It does not create two independent copies. The prepared Qwen two-device recipe needs a separate runtime load test before its performance can be claimed.

Use [cluster.example.json](cluster.example.json) as a generic site example. Its `spark-user` account, example domain, and documentation IP addresses are placeholders. Replace them with your site values before installation. Keep the real configuration outside the repository. Edit the file on Spark 1. Spark 1 derives service ownership from the model placements and sends each read-only probe over the direct link. Copied service lists do not control model detection. The small node files on Spark 2 support the ComfyUI service guard. They are not another model authority.

```sh
sparky config show
sparky config validate
sparky plan load qwen --on both
sparky config placement qwen both
sparky load qwen
```

A placement change applies on the next load. Use a plan first to see which idle models must leave memory. Resource plans never stop services.

## Resource control

The controller runs as `sparky-controller.service` on Spark 1. The existing llama-swap manager handles explicit model loading and unloading. Inference cannot start a stopped model. Idle unloading is disabled. Sparky derives its compatibility matrix from the central model placements. Nemotron on Spark 1 and Qwen on Spark 2 can run together. The two-device models require both nodes.

The controller tracks node reservations for LLM requests and ComfyUI jobs. Conflicting work waits. Active LLM requests and active image jobs finish before a conflicting model is unloaded. Unknown activity prevents unloading. Idle ComfyUI caches can be released. Protected Parakeet containers stay loaded. These are software admission checks. They do not create a hardware GPU memory cap.

A ComfyUI custom node reserves the device for the full execution of each job. This also covers jobs submitted through the existing ComfyUI UI. Existing services, workflows, assets, and image outputs stay in place. The controller uses no kernel, driver, or global memory changes.

```sh
sparky status
sparky devices
sparky models --installed
sparky plan load glm
sparky load glm
sparky unload glm
sparky unload qwen --force          # Cancel Qwen requests and stop Qwen
sparky load deepseek --force        # Cancel conflicting requests, stop their models, then load
sparky run comfyui workflow-api.json --on spark02
```

Language models use explicit load and unload commands. Inference requests never
load a stopped language model or replace a loaded one. If the selected model is
not ready, the gateway returns an error that asks the agent to ask the human to
switch the model. The error tells the agent not to load or unload models automatically.
`spark-auto` selects a ready target. It returns an error if no target is ready.
The controller sends admitted inference directly to the reserved model server.
llama-swap still handles explicit model loading and unloading. The controller
keeps request limits, device reservations, streaming, and forced cancellation.
Language model idle unloading is disabled. The legacy `idle_ttl_seconds` field
does not enable it. Exclusive image jobs wait until you explicitly unload any
language model that occupies their device.

`load --force` and `unload --force` support language models. They cancel conflicting manager requests and waiting device reservations. They stop the affected model services before the requested operation. Requests can fail and partial replies can end. Cancellation has a 30-second drain limit. Service shutdown and model startup can take longer. Protected models and active image jobs still block the operation. Without `--force`, active work finishes first.


`sparky run` starts ComfyUI if needed. A separate `sparky load` command is optional.
For image checkpoints, `load` prepares the service; the workflow loads the weights.

Shared image execution is opt-in. The default policy keeps GPU image jobs exclusive.
To share with an existing language model, set `comfyui_exclusive_with_llms` to `false`,
list approved model IDs in `comfyui_shared_llms`, and set a positive
`comfyui_min_available_gib`. The controller checks host `MemAvailable` on the image
node. If memory is low, it releases only idle ComfyUI caches, checks again, and waits
if necessary. It never unloads an LLM to admit a shared image job. LLM startup and
explicit unload still require exclusive reservations. These checks are not a hard
memory limit. Test each image workflow and size before enabling it for clients.

The optional image API uses the installed ComfyUI engine. Enable `image_api` only on
`flux2-klein-4b` with `{"enabled": true, "node": "spark02"}`. It exposes
`POST /v1/images/generations`, uses the FP8 checkpoint with four sampling steps,
and returns `data[0].b64_json`. It supports one image per request at 512, 768, or
1024 square pixels. It does not support image editing or streaming. Gateway image
requests run one at a time. Use a separate Bifrost image provider to preserve the
language provider's request slots. Existing ComfyUI UI and API workflows also use
the job guard. No ComfyUI browser session is needed.

`workflow-api.json` must be a ComfyUI API workflow. The installed checkpoints include image models. No dedicated video checkpoint was found during inventory.

## Harness configuration

`GET /sparky/catalog` exposes the central names, aliases, context limits, output budgets, modalities, and selected devices through the existing keyless inference ingress. The client fetches this catalog before each launch. It has no local model settings to override it. `spark-auto` keeps its existing gateway alias and uses the common context budget.

Speech services appear in the catalog's `audio_services` field and in `sparky models`.
Use `sparky config show kokoro` for the speech model, Bifrost API URL, default voice,
available voices, formats, and CPU placement. `sparky models --installed` and
`sparky status` include the running audio container. Speech services stay separate
from the language models offered to agent harnesses. The public speech metadata
does not include backend addresses or credentials.

The local file `~/.config/sparky/client.json` stores only gateway, SSH, client preferences, and executable paths. Local `models` settings are not supported; model settings belong on Spark 1. See [config.example.json](config.example.json). `SPARKY_CONFIG` selects another client file. `SPARKY_STATE_DIR` selects another local state directory. Use `--offline` explicitly to accept cached discovery data.

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

Python 3.11 or later is required. The client uses Rich for terminal tables and colors. The node archive includes the terminal renderer, so it needs no new system packages. The client uses `ruamel.yaml` to preserve Hermes comments. The node archive includes a portable YAML parser to validate native manager configuration. It does not require host Python packages.

```sh
uv tool install .
sparky --help
make test
uv run --locked make node-helper
```

Copy `dist/sparky.pyz` and `tools/install_node_helper.py` to each Spark. Run
`python3 install_node_helper.py --archive sparky.pyz` on that Spark. The installer
keeps a backup and preserves archive offsets used by a running controller. Stage the cluster configuration on Spark 1. The explicit node installer creates the controller service and ComfyUI guards. It waits for image queues to drain before it restarts each ComfyUI service. It preserves running LLM and Parakeet containers. It keeps private backups of changed server files.

```sh
sparky node install --dir /path/to/staged-config
```

The staged folder must contain `models.json`. The first install creates the central file. A later install preserves an existing central file. The `node` command is the private machine protocol. Normal users do not need it. Its output stays JSON for the manager and SSH transport.

`sparky` is the sole command. Old `spark`, `spark-models`, and `opencode-spark` commands are removed. There are no compatibility wrappers.

The interface review is in [docs/usability-review.md](docs/usability-review.md). The full correctness review is in [docs/correctness-review.md](docs/correctness-review.md). Current checks are in [docs/VALIDATION.md](docs/VALIDATION.md).

Focused verification tools cover resource planning, node reservations, guarded ComfyUI execution, numeric recipe rendering, native harness parsers, and synthetic harness requests. Synthetic tests do not prove model quality. Live checks must distinguish a prepared recipe from a loaded and verified model.


## OCR service

OCR management uses administrator SSH to Spark01. Update Sparky on the client and
Spark01 before using these commands. It does not require an update on Spark02.
The client file must have `admin_ssh_host`, an absolute `remote_root`, and the
HTTPS Bifrost `gateway`. The cluster root must already contain the Bifrost setup.

Spark01 needs Git access to the private OCR repository, Docker Engine, the Compose
plugin with GPU support, NVIDIA Container Toolkit, and passwordless `sudo -n docker`
for the administrator account. Git authentication stays on Spark01. Do not put
credentials in the repository URL. OCR bootstrap downloads pinned model weights.

```sh
sparky ocr install --repo git@github.com:OWNER/PRIVATE-OCR.git --ref main
sparky ocr status
sparky ocr stop
sparky ocr start
sparky ocr status --json
```

The first install requires `--repo`. Use a commit hash with `--ref` for a fixed
source revision. Later `sparky ocr install` reuses the saved repository and ref.
It checks out that ref again. For a branch, this installs its current commit.
For a commit hash, it keeps that exact commit. Use `--rebuild` to rebuild images;
otherwise an existing image for the package version is reused.

Install builds missing images, prepares models, creates private service settings,
starts OCR, and registers its Bifrost provider and MCP tool. It preserves existing
OCR credentials and custom model-cache paths. It refuses to replace a Mistral
provider owned by another service. A failed activation attempts to restore the
previous OCR service. Build and registration details stay in a private install log
on Spark01. JSON stdout contains one result. Interrupted remote work may continue;
check `sparky ocr status` before retrying.

Files are stored under `<remote_root>/ocr/`: `service.env` holds the private token,
`sparky-install.json` identifies the selected source revision, and `releases/`
contains source checkouts and install logs. Existing `ocr/source/deploy/.env`
settings are adopted on the first managed install. Previous source releases remain
on disk for recovery. They are not removed automatically.

Status reads container state and the HTTP health endpoint. It does not run OCR,
load models, or change services. `api_ready` checks the API, not OCR accuracy or a
warm GPU worker. Start uses the installed image and settings. It does not build
or download weights. Stop stops only the `spark-ocr` container and releases its
worker memory. Model files and Bifrost registrations stay in place. Requests while
OCR is stopped fail; they do not start it. Run `sparky ocr start` explicitly.

These commands do not load, unload, or restart language models. OCR is a separate
Docker service. Resource admission for language and image models does not account
for OCR allocations. A GPU allocation failure produces an OCR error. This interface
does not add a new scheduling or model-switch policy.
