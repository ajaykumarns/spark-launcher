# Image memory profiles

ComfyUI selects and loads image weights from each workflow request.
Sparky selects device placement and the service memory limit.
The image helper does not change either setting.

For large image jobs, select the tested single-Spark language recipe first:

```sh
sparky plan load qwen3.8-flash-next --on spark01
sparky config placement qwen3.8-flash-next spark01
sparky load qwen3.8-flash-next
sparky image-profile dedicated --dry-run
sparky image-profile dedicated
```

The dedicated image profile requires Flash Next to be ready on Spark01.
It refuses a language model on Spark02. It also refuses active or queued image work.
It drains language requests before it changes limits.
It releases idle ComfyUI models. It changes MemoryHigh to 40 GiB and MemoryMax to 48 GiB.
It keeps the image service swap limit at zero.
It sets image admission to 96 GiB of host available RAM.
It verifies the effective service limit through both systemd and the ComfyUI API.
It does not restart the language model.
It restarts only idle ComfyUI when a lower memory limit or low available RAM requires it.
This releases CUDA process state that model-cache cleanup cannot release.
These limits persist across service restarts and device restarts.

The tested ComfyUI flags remain reserve-vram 12, cache-none, disable-pinned-memory, and disable-smart-memory.
Use the shared `comfyui-spark` skill for tested workflows and prompt-specific cancellation.
The BF16 workflows require the dedicated limits. The helper refuses them under the shared profile.
The service limit does not cover every GPU allocation. The image helper also checks whole-host RAM.
A profile is not permission to run arbitrary larger batches, videos, or resolutions.

To return to the shared image profile and the dual-Spark language recipe:

```sh
sparky image-profile shared
sparky config placement qwen3.8-flash-next both
sparky load qwen3.8-flash-next
```

The shared profile restores MemoryHigh=14 GiB, MemoryMax=16 GiB, zero service swap, and 21.5 GiB of host available RAM for image admission.
Run `sparky image-profile show --json` to read the effective service limit.
Run `sparky status` to check actual model placement.
A saved placement takes effect on the next load. It is not proof of a running deployment.
