# Validation for Sparky 0.6

The [correctness review](correctness-review.md) records the findings and removals. This report replaces the obsolete 0.1.0 instructions.

## Software checks

```sh
uv sync --locked
uv run --locked make test
uv run --locked make node-helper
```

The regression suite covers device ownership, placement changes, protected audio, ComfyUI reservations, interrupted startup, all-rank shutdown, lock recovery, recipe matching and rollback, maintenance fences, HTTP framing and streaming, harness settings, output formats, contexts and memory attribution. It uses temporary files, local servers and controlled device transports.

The node archive also passed with Python site packages disabled. It includes Rich and portable YAML support. It contains no host-specific native extensions.

## Native and live checks

- Installed Codex and OpenCode parsers accepted all seven gateway routes and their context limits.
- The installed Hermes resolver selected the remote provider for all seven routes. The check used a temporary configuration copy.
- Installed Claude Code and Codex completed streaming requests against an isolated test server.
- The new schema accepted the installed central configuration.
- All 13 per-node recipe placements passed the stricter renderer. A recording stub replaced its write function. File hashes remained equal.
- The rebuilt probe ran on both Sparks from a temporary archive. It reported Nemotron on Spark 1, Qwen on Spark 2 and Parakeet running. Whisper was stopped.

## Scope

Synthetic replies prove transport and configuration, not model quality. Recipe checks do not prove a fresh load, a new context limit, a new placement or peak GPU memory use. The review kept current model settings and assets.

Client installation and controller activation are separate steps. Replacing an archive does not update modules already loaded in a controller process. Controller activation must wait for active calls and image work to finish. Model and audio processes can remain loaded.
