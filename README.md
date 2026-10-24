# Spark launcher

One Python command for Claude Code, Hermes, Codex, OpenCode, and existing DGX Spark model controls.

```sh
spark
spark claude --model nemotron
spark hermes --model glm
spark codex --model deepseek --project ~/dev/project
spark opencode --model qwen
```

`spark` without arguments opens a simple numbered menu. A launch uses your current directory unless you select another project. Harness permissions, chat history, MCP servers, and unrelated providers stay in place.

## One model catalog

The gateway's `GET /v1/models` is the authority for deployed model IDs and **context windows**. The launcher fetches it before each launch. It does not copy context constants into its source or quietly substitute a smaller limit. `spark-auto` uses the minimum advertised context among the language models in the catalog. This is conservative because its route can change. Models without usable context metadata are omitted.

`~/.config/spark/config.json` stores the site URL and model labels, aliases, client output budgets, modalities, and device requirements. Import these once from your current OpenCode configuration. These are client budgets, not claims about a model's published maximum output. Context limits remain live server data.

```sh
spark init --gateway https://spark.example:8443 \
  --admin-host spark01-admin --worker-host spark02 \
  --remote-root /home/your-user/spark-cluster \
  --manager-url http://your-tailnet-address:8000 \
  --admin-url https://spark.example \
  --import-opencode ~/.config/opencode/garuda.json
```

See [config.example.json](config.example.json). The site file contains no administrator or provider keys. `SPARK_CONFIG` selects another site file. `SPARK_STATE_DIR` selects a different cache and launch-record directory. Live discovery fails clearly when the gateway is unavailable. Use `--offline` explicitly to accept the last cached catalog.

## Harness adapters

| Harness | Connection and configuration |
|---|---|
| Claude Code | Bifrost `/anthropic`; explicit model and helper-model mappings; `CLAUDE_CODE_MAX_CONTEXT_TOKENS`; long request timeout |
| Hermes | Existing `providers.spark-gateway` block; complete current model catalog, context limits and timeouts; native `--provider` and `--model` flags |
| Codex | A custom `spark` provider using `/v1/responses`; explicit context and compact threshold; a catalog generated from the installed Codex schema with Spark IDs and no cloud model upgrade mapping |
| OpenCode | `OPENCODE_CONFIG_CONTENT` supplies `/v1`, current model metadata, and the chosen primary/helper model. Persistent OpenCode configuration is not rewritten. |

Some SDKs require a non-empty token even for a keyless endpoint. `spark-keyless` is a fixed **client placeholder**, not a gateway secret. Real provider credentials stay on the server. Existing cloud sign-in files are not removed.

Select another model by starting a new `spark HARNESS --model NAME` invocation. In particular, Claude Code's context environment variable applies to the launch; switching to an unrelated custom model inside that process cannot update that variable. The default Opus, Sonnet, Haiku, and helper mappings all point to the selected Spark model.

Hermes has no verified independent configuration-overlay path in the installed release. The launcher therefore updates only its existing Spark provider block. It takes a private backup, keeps YAML comments, checks that unrelated settings remain equal, and uses a lock plus atomic replacement. It does not change the default model or reset a running Hermes gateway. Existing daemon chats need their own native model refresh; the launcher affects the CLI process it starts.

Codex gets `model_context_window` and an explicit auto-compact threshold from `compact_percent` in the site file. Other harnesses keep their own compaction policy. Context window and summary threshold are different settings. The launcher does not promise identical summary timing across different clients. The initial Codex compact threshold is 95 percent. Set `compact_percent` in the site file to a value from 50 to 95 if you want a different threshold.

## Commands

```sh
spark models                         # Live model IDs, contexts and output budgets
spark models --json
spark models --offline               # Explicit use of cached metadata
spark claude --model nemotron --dry-run
spark codex --model nemotron --prepare # Generate configuration; do not run inference
spark sync                           # Refresh catalog and Hermes's Spark provider
spark doctor                         # Discovery and installed binaries only
spark logs                           # Last local launch metadata records
spark ui                             # Open existing gateway UI
spark status                         # Existing manager state, over administrator SSH
spark devices                        # Actual host MemAvailable, GPU and containers
spark admin load glm
spark admin unload nemotron
spark admin ssh spark02
```

Use `--` before native harness arguments. Set the model through `spark`, so the context and route agree with its launch record:

```sh
spark claude --model qwen -- --continue
spark opencode --model glm -- run 'Explain this project'
spark codex --model nemotron -- exec 'Explain this project'
```

The launcher does not enable permission bypasses. It does not run inference tests, change drivers, tune memory, preload every model, or restart Docker services during a normal launch.

## Existing services and gateway support

Bifrost still handles protocol conversion and SQLite call logs. llama-swap still owns loading, the three-hour idle timer, node sharing and exclusivity. The launcher adds no permanent daemon.

The current custom OpenAI-compatible model provider needs two setup changes for these harnesses:

1. Allow only `/anthropic/v1/messages` and `/anthropic/v1/messages/count_tokens` through the existing inference locations. Keep administrator routes protected.
2. Configure Bifrost's native Responses-to-Chat fallback for the model provider. This is needed for models whose server implements Chat Completions but not native Responses. Responses streaming, functions, and tool results are converted by Bifrost, not reimplemented in this launcher.

The pinned Bifrost `transports/v2.3.2` supports this through `custom_provider_config.allowed_requests`: native `responses` and `responses_stream` are false while Chat Completions remains true. The migration preserves all other existing operation flags. If there were no flags, it materializes the pinned release's default-allowed operations first. Review this list when upgrading Bifrost.

After installing the Python node helper on the head node:

```sh
spark admin enable-harnesses
```

This explicit administrator operation takes private server-side backups, updates the provider through its existing API, validates nginx, and performs a graceful nginx reload. It preserves bind-mounted file inodes and rolls back on a configuration failure. It does not restart a model or audio container. Model limits and recipes do not change.

`spark status` and `spark admin load/unload` replace the old `spark-models` script. The node helper reads the manager credential on the node and never sends it back over SSH. Device and administrator operations retain your existing SSH identity checks. Normal harness launches do not use SSH. This launcher is not an OS security boundary for agents running as your own user.

## Logs

The existing gateway remains the LLM call-log authority. Claude Code, Codex, and OpenCode get `X-Client-ID` and `X-Session-ID` request labels. Hermes gets a stable `X-Client-ID` in its provider configuration. Local launch records contain model, context, project, timestamps and exit status. They contain no prompts, tokens, API keys, or copied environment. Backups may contain existing configuration credentials and are private, outside this repository.

## Install and validate

Python 3.11 or later is required. The launcher has one library dependency: `ruamel.yaml`. It preserves Hermes YAML comments and unrelated settings. HTTP, subprocesses, command parsing, and node controls use the Python standard library.

```sh
uv tool install .
spark --help
uv run make test
make node-helper
```

`uv tool install` installs an isolated Python environment and exposes the `spark` command. `pipx install .` is also supported. The selected harness must already be installed.

`make node-helper` builds `dist/spark-node.pyz`. Copy that file to `~/.local/bin/spark` on each Spark and make it executable. Node commands need only the existing Python standard library. They do not install packages into the system Python. The same source implements local and remote controls.

```sh
uv run python tools/verify_parsers.py
```

Run this parser check with `uv run python tools/verify_parsers.py` from this repository. It checks the installed Codex and OpenCode parsers against live discovery metadata. It does not submit an inference request. `tools/verify_hermes.py` checks the installed Hermes resolver in a temporary configuration. Run it with the Hermes venv Python and `PYTHONPATH=src`. `tools/verify_mock_harnesses.py` checks actual Claude Code and Codex HTTP requests against a local synthetic server. Synthetic checks do not prove model answer quality.

Keep `spark-models` and `opencode-spark` as compatibility wrappers after installation. Back up their previous contents before replacing them:

```sh
# spark-models
exec "$HOME/.local/bin/spark" "$@"

# opencode-spark
exec "$HOME/.local/bin/spark" legacy-opencode "$@"
```

## References

- [Claude Code gateway setup](https://code.claude.com/docs/en/llm-gateway-connect)
- [Claude Code custom context windows](https://code.claude.com/docs/en/model-config)
- [Codex provider configuration](https://learn.chatgpt.com/docs/config-file/config-advanced)
- [Codex gateway protocol requirements](https://learn.chatgpt.com/docs/enterprise/gateway-compatibility)
- [OpenCode runtime configuration](https://dev.opencode.ai/docs/config)
- [Bifrost Claude Code integration](https://github.com/maximhq/bifrost/blob/dev/docs/cli-agents/claude-code.mdx)

Harness formats change. The launcher uses the installed Codex catalog schema rather than embedding one particular vendor model catalog. Validate the parser checks after harness upgrades.
