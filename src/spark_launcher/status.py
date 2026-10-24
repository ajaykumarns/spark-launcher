"""Short terminal view of the controller status. No device operations."""


from .presentation import table, memory, heading, note

def format_status(data):
    devices = []
    for node, state in data.get("nodes", {}).items():
        mem = state.get("memory_bytes", {})
        comfy = data.get("comfyui", {}).get(node, state.get("comfyui", {}))
        if not comfy.get("reachable"):
            image_state = "unavailable"
        else:
            image_state = comfy.get("phase") or ("running" if comfy.get("running") else "idle")
            image_state += f" ({comfy.get('pending', 0)} queued)"
        devices.append((node, memory(mem.get("MemAvailable")), memory(mem.get("MemTotal")), image_state))
    models = []
    for name, state in data.get("models", {}).items():
        if state.get("kind") not in ("llm", "audio"):
            continue
        active = state.get("active_requests", 0)
        if state.get("kind") != "llm" or data.get("activity_known") is False:
            active = "-"  # The controller does not count audio transcription calls.
        models.append((name, state.get("state", "unknown"),
                       ", ".join(state.get("nodes", [])) or "-", str(active)))
    admission = data.get("admission", {})
    waiting = admission.get("waiting", [])
    lines = [table(("DEVICE", "AVAILABLE RAM", "TOTAL RAM", "COMFYUI"), devices, title="Devices"), "",
             table(("MODEL / AUDIO", "STATE", "LOADED ON", "LLM CALLS"), models, title="Models and audio"), ""]
    lines.insert(0, heading("Spark cluster status") + "\n")
    if waiting:
        lines.append(note("Waiting: " + ", ".join(item["workload"] for item in waiting), "yellow"))
    else:
        lines.append("Device requests waiting: 0.")
    if admission.get("config_transition"):
        lines.append(note("New work is paused for a configuration change.", "yellow"))
    protected = [name for name, state in data.get("models", {}).items() if state.get("keep_loaded")]
    if protected:
        lines.append(note("Kept loaded: " + ", ".join(protected) + ".", "green"))
    if data.get("activity_known") is False:
        lines.append(note("Active LLM call counts are unavailable.", "yellow"))
    lines.append(note("Use sparky status --json for full details."))
    return "\n".join(lines)
