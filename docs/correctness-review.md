# Correctness review

The review covered every source module, support tool, test and configuration example. It used four rules: Spark 1 owns model settings; reservations cover actual devices; failures leave a recoverable state; commands expose only supported behavior.

## Confirmed faults and fixes

| Area | Fault and correction |
|---|---|
| Startup | SIGTERM handling began after rank startup. It now covers startup and cleans up partial loads. Shutdown attempts every rank even if one node fails. |
| Actual placement | A disabled desired recipe could block stopping the actual service. Stop now reads actual placement first. Explicit unload reserves both actual and desired devices. |
| Placement races | A config edit could change nodes between synchronization, reservation and startup. These steps now use one configuration generation. |
| Maintenance | An interrupted drain could leave admission blocked. Reload could clear an administrator fence. Reload now owns a separate pause flag and clears only that flag. |
| Recipe matching | A match in one file hid a missing match in another. A Python argument-list match skipped a later worker override. Every binding must now match, including explicit overrides. |
| Recipe writes | A later write failure left earlier files changed. Written files now roll back. Duplicate path bindings are rejected. |
| State discovery | Copied service lists could miss new placements. Container query failure could look like stopped services. Units now come from model settings. Worker probes receive current metadata over stdin. Container queries read selected state fields and fail explicitly. |
| Compatibility | Each inference request could examine all `2**N` model subsets. Two exclusive devices permit at most two LLMs. The code now generates disjoint pairs and remaining single-model sets. |
| Validation | Invalid service names, paths, placement types, budgets and numeric values reached runtime code. Context could be set twice. Validation now rejects these cases and case-insensitive alias collisions. |
| Protected models | Plans could propose an unsupported audio unload or evict a protected LLM. Both cases now fail before device actions. Parakeet remains protected. |
| Idle timeout | The central idle policy was ignored. It now supplies manager `globalTTL`. The current 10800-second value stays unchanged. |
| Auto routing | Auto context included unrelated models. A disabled LLM could break the catalog. Auto now uses configured targets and authoritative gateway metadata. Disabled placements are skipped. |
| HTTP methods | GET requests with bodies could reach mutation handlers. Methods are now checked. Bodies must be JSON objects. Fence values must be Boolean. |
| HTTP framing | Duplicate lengths, conflicting chunk headers and truncated chunks or trailers were unsafe. Framing is now checked, body reads are bounded and JSON connections close after the reply. |
| Proxy headers | Headers named by `Connection` crossed the proxy. They are now removed in both directions. |
| Audit failure | A SQLite error after lease release could cause a second HTTP error response. Release now remains complete without replacing the response. |
| Process locks | A crash left existence-only locks that blocked future commands. OS file locks now release on process exit. Their inode stays in place to prevent waiter races. |
| Gateway rollback | A failed provider restore prevented file restores. Interrupted setup could skip rollback. Every restore is now attempted independently. Incomplete recovery is reported. |
| Harness settings | OpenCode discarded unrelated process providers and MCP settings. Compact model flags and review overrides could bypass routing. Process settings now merge. Extra routing flags are checked. Native prompt delimiters are respected. |
| Results | New Hermes settings could report current instead of updated. Child signal exits could produce invalid shell codes. Both results are corrected. |
| Packaging | The lock still described 0.1.0 and omitted dependencies. YAML relied on host installation. The lock is current. The archive includes portable YAML and excludes native extensions. |
| Installer | A staged root could differ from the installed root. Dual-recipe edits could silently miss flags. Both conditions now fail before service changes. |

## Removed code

- Unused client `models` settings. Model settings belong on Spark 1.
- Unused private `node lifecycle` command. The manager calls the function directly.
- Native Responses and embeddings routes in the controller. The public Responses API keeps Bifrost's translation to Chat Completions.
- Unused general audio/ComfyUI compatibility helper. The matrix contains LLMs only.
- Duplicate service and container lists and the ineffective exclusivity switch in the example configuration.
- Redundant help/version verification tool that could modify live Hermes settings.
- Unused imports, a duplicate install loop, unused state variables and obsolete compatibility instructions.

The user cancelled gateway-based status. It was not added during this review.

## Evidence and limits

See [VALIDATION.md](VALIDATION.md). The review changed no model context, GPU memory setting, driver or model asset. Synthetic checks do not prove answer quality. The prepared Qwen dual-device recipe still needs its own runtime load test before its performance can be claimed. This review does not prove that every possible fault is absent.
