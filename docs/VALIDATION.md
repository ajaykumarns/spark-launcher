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

## Current Go native verification (2026-11-02)

- Source revision: edb602d. Final actual full Go race suite: 547 pass, 2 normal skips, 81.865s, no exclusions. `go vet` pass.
- Cross-build pass: CGO 0 Darwin ARM64 and Linux ARM64.
- 140 concurrency race checks, 20 repeats: pass.
- The installed systemd parser validated the real units. The Linux compiled installer rejected an AMD64 source without creating the target — direct proof of the architecture check (Go tests could not run on the Spark because Go is not installed there; the one compiled Linux-only check was skipped for that reason, while actual Linux runtime, unit-parser, and installer-model proof came from the Spark runs).

Live native state (Sparky CLI runs on the Mac and both Sparks; the controller and native supervisor run on the head Spark only; Go node helpers run on both Sparks):

- Go legacy parity was verified first; the installed Sparky now runs `lifecycle: "native"` (the only configuration difference from the prior setup).
- llama-swap is inactive and disabled; no 8000 listener. The same GLM 5.3 flash model is ready on the previous placement.
- Native first load and ready-load idempotence pass. Public OpenCode Spark Auto returns `NATIVE_SPARK_AUTO_OK`.
- SSE cancellation: backend idle in 0.255s, zero leases. A controller restart kept the lifecycle record generation, supervisor, and both rank PIDs unchanged.
- Native unload: physical process exit, port closed, record `Stopped`; the same GLM reload then created a new generation and became ready, followed by a successful fresh completion.
- Engine Sparky probe passes the CPU guard (`SPARKY_GUARD_OK`) with the model ready.
- The seven protected OCR/audio/gateway containers kept their IDs and start times; ComfyUI services were not restarted. Private backups retained.
- Scope: other LLM recipes, image, and GPU model placements were not loaded (current model-only constraint). Relevant unit and contract tests pass; not all models are claimed live-tested. Python ComfyUI engine plugin and OCR engine bootstrap stay engine-owned exceptions. The Python Sparky trees remain reference/rollback and are unused by the installed Sparky binaries.

## Legacy Python reference

The Python-era checks below are kept as a reference and are superseded by the current Go native verification above.

## Activation check on 2026-10-24 (Python reference)

The server core (0.6.0) was activated after the controller became idle. The client-only OpenCode correction (0.6.1) was installed on the Mac. No second controller restart was needed for that client change.

Bounded keyless HTTPS calls to the already loaded Qwen and Nemotron models passed through the updated controller. Each call generated at most 16 tokens. These calls verified transport and routing, not answer quality.

Before and after snapshots showed equal container IDs, start times, model runtime files, manager settings and ComfyUI process identities. Only the controller process changed. Parakeet remained loaded. Whisper remained stopped. Nemotron stayed at 524288 context tokens. Qwen stayed at 262144. Gateway administrator authentication still rejected unauthenticated requests.

The final regression suite passed 107 tests. A native OpenCode check also preserved a custom configuration file and its disabled MCP entry while selecting the Spark model.
