# Human interface review

This review covered every public command, its help, normal output, JSON output, errors, waiting behavior, and common failure cases. The review also checked whether the output makes configuration, running state, and completed work distinct.

## Findings and fixes

| Finding | Change |
|---|---|
| Resource plans printed internal JSON. | Numbered steps explain waits, unloads, memory release, and protected services. Plans state that they changed nothing. |
| Configuration inspection required reading the whole file. | The overview is a table. `config show NAME` shows per-placement limits and parameters. |
| Configuration validation printed the whole file. | It now prints validity, entry count, and the central file path. JSON validation returns a small result. |
| Placement changes could look like immediate service moves. | The result states that running services were not moved and gives the load command to apply the change. |
| Load and unload results were JSON. | They now give a short result. ComfyUI load explains that weights load when a workflow executes. |
| Long model operations had no visible progress. | Human mode prints the requested action and a message every 15 seconds while waiting. JSON mode stays quiet. |
| The user could not see which models a load would evict. | A human load first reads its plan and names the conflicts before it submits the operation. |
| A prepared recipe could look verified. | Plans and human loads label a placement whose live model load has not been verified. |
| Workflow submission could look like completed image generation. | The result says submitted, gives the job ID and node, and explains that execution may wait. |
| A normal ComfyUI UI export caused a poor server error. | The client checks the API workflow structure before submission and explains which export format is needed. |
| Missing or malformed workflow files gave low-level errors. | Errors identify the file and JSON line and column without dumping its contents. |
| Device output was separate JSON documents. | Human output shows device summaries. JSON mode returns one object for both devices. |
| One failed device could hide the other device's result. | Device queries run independently. Partial results remain visible and the command exits with failure. |
| Missing GPU or Docker queries could disappear silently. | Device results include readable warnings. |
| Shared CPU/GPU memory could be double-counted. | Device output explains that the RAM and GPU figures refer to shared memory. |
| Local logs required a working client configuration. | They work without a client file or gateway connection. |
| Logs were a raw JSON list, with no empty state. | They now show recent launch rows, local times, exit results, and a clear empty state. |
| One malformed log file could hide all history. | Bad files are skipped and counted. Valid records remain readable. |
| Missing finish data could be confused with a running process. | Logs say “not finished” instead of claiming that the process is still running. |
| Harness previews dumped argument lists and environment objects. | Human previews show the selected model, context, gateway, project, executable, and generated files. Full plans require `--json`. |
| Preview and preparation looked similar. | Output states whether settings were written and that no harness was started. |
| JSON support was limited to two commands. | Every public data command now supports it, including nested configuration commands. |
| Progress or remote output could break script parsing. | Public results are collected before rendering. JSON stdout contains one object. Errors use JSON stderr and nonzero status. |
| A native harness `--json` flag could be mistaken for a Sparky flag. | Arguments after `--` stay with the native harness. Sparky JSON mode is restricted to launch previews and preparation. |
| `ui --json` could cause an unwanted browser action in a script. | It returns the URL without opening a browser. |
| Help omitted command descriptions and practical examples. | Root help uses short descriptions and examples. Subcommand options state their purpose. |
| Duplicate load/unload paths and an undocumented launch alias added uncertainty. | The public paths are `load`, `unload`, and direct harness commands. No compatibility aliases were added. |
| Model aliases were case-sensitive and typos gave little help. | Names are matched without case. Errors suggest close model or command names. Ambiguous model errors list the matches. |
| An unsupported placement gave little guidance. | The error lists available placements. |
| Offline output could appear current. | Cached data is explicitly labeled. Missing caches explain how to create one. |
| Missing executables and `~/...` executable paths behaved poorly. | Paths expand before access checks. Errors say how to install or configure the executable. |
| Malformed client or central settings could cause a traceback. | Common type and JSON errors now give an actionable message. |
| HTTP errors hid safe resource-controller explanations. | Only the local controller's safe error string is carried through SSH. Admin and backend response bodies stay private. |
| Tailscale sign-in links could be hidden until a long load timeout. | The SSH reader checks partial stderr every five seconds and returns the known identity-check URL. Raw diagnostics stay private. |
| Diagnostic checks stopped at the first failure. | `doctor` reports the gateway and all requested harness checks, then returns an appropriate exit status. |
| Tables could exceed a normal terminal width or contain control characters from names. | Shared formatting wraps cells without dropping values. Very narrow views use labeled rows. Terminal control characters are removed from human table values. |
| Image checkpoints were listed as stopped even though file presence cannot prove memory state. | Installed inventory labels them “on demand.” Status focuses on actual LLM, audio, and ComfyUI state. |

## Check scope

The automated suite covers human and JSON command paths, resource scheduling, protected audio, ComfyUI execution guards, recipe parameter rendering, script errors, aliases, partial failures, and narrow terminals. CLI tests use controlled transports for mutation commands. They do not stop live models.

Live verification uses read-only commands, existing model metadata, local launch previews, and a protected Parakeet unload refusal. It does not reload models, change placement, submit a GPU workflow, or restart the manager, controller, Parakeet, or ComfyUI.

The private `node` protocol still uses JSON. Public commands consume that protocol and render a human result. Native harnesses retain their own UI and output modes. Their configuration formats are not replaced.

This is a command-line review. No claim is made that a GUI, every terminal emulator, or every screen-reader combination was physically tested. Source recipe changes do not count as a live test of a new model placement.

## Visual correction

The first review treated aligned text as a finished terminal interface. It was not enough. Version 0.4.0 uses Rich bordered tables, section headings, detail cards, and state colors. Running states are bright green. Stopped states are dim. Warnings and errors have their own colors and explicit text.

The display was rendered and inspected with live status data at 80 and 100 columns. Automated checks cover 60, 80, 100, and 120 columns, interactive color, redirected text, NO_COLOR, explicit color settings, and JSON isolation. Model IDs stay intact in the tables. Very narrow terminals use labeled cards. The node artifact bundles the renderer. No host package or service change is needed.
