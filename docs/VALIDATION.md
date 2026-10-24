# Spark launcher 0.1.0

The launcher is written in Python. It is installed as `spark` on the Mac. A Python standard-library node helper is installed on both Sparks. No launcher daemon is used.

## Verified

- All 16 software tests passed.
- The installed Codex and OpenCode parsers accepted all seven model contexts.
- The native Hermes resolver selected the remote gateway for all seven models.
- Actual Claude Code and Codex clients completed streaming HTTP requests against a local synthetic server. Model IDs and client/session logging labels matched the launch settings.
- All four installed harnesses launched their help or version command with exit code 0. The old `opencode-spark --model ...` and `spark-models list` forms also passed.
- Keyless gateway discovery passed. Empty Messages, token-count, and Responses requests returned 400. The real admin provider endpoint returned 401. Unknown UI paths returned HTML, not provider configuration.
- Model configuration hashes, container IDs, and container start times were equal before and after setup on both nodes. Nemotron and Qwen stayed loaded. Parakeet stayed running. Whisper stayed stopped.

## Installed changes

- One private site file at `~/.config/spark/config.json` holds gateway URLs, aliases, labels and client metadata. Context windows come from live gateway discovery. Explicit offline mode uses the cached discovery result.
- Nemotron remains at 524288 tokens. Qwen remains at 262144 tokens. Other server model settings are unchanged.
- Codex gets its own Spark provider and generated model catalog. Its initial compact threshold is 95 percent. This is a client summary threshold, not a model context limit.
- Claude Code maps its primary and helper roles to the selected Spark model.
- Hermes updates only its owned Spark provider. Its other providers, defaults and MCP settings remain in place.
- OpenCode gets a per-process provider. Its persistent config files remain in place.
- The old local commands are compatibility wrappers. Their original contents have private backups.
- Bifrost exposes the two Anthropic inference routes through the existing keyless inference path. Its native Responses-to-Chat fallback is enabled for the existing model provider. Admin routes remain separate.
- The gateway change used private server-side backups, validation and a graceful nginx reload. No model or audio container was restarted.

## Scope of validation

No fresh model inference, tool execution against a model, load test, or quality benchmark was run. The checks validate the launcher, native client settings, synthetic HTTP exchanges, gateway boundaries and service preservation. Existing model quality results were not rerun.

## Use

```sh
spark
spark claude --model nemotron
spark hermes --model glm
spark codex --model deepseek
spark opencode --model qwen
spark models
spark devices
```

Start a new launch to change a model. Native in-session model changes can retain the first model's context settings in some harnesses.
