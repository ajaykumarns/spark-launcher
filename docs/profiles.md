# Cluster profiles

A cluster profile lists the AI capabilities you want available. Under each
capability, list its providers and their placements. The catalog in
`models.json` defines the installed recipes and supported placements.

Store profiles in `config/profiles.yaml` beside `models.json` on the head Spark.
For example:

```yaml
version: 1
profiles:
  everyday:
    capabilities:
      chat:
        - glm: both
      decisions:
        - knowline: spark01
      images:
        - comfyui: spark02
      speech:
        - kokoro: spark01
      transcription:
        - parakeet: spark02
      ocr:
        - spark-ocr: spark01
```

Use `sparky profile plan everyday` to preview the switch. Use
`sparky profile apply everyday` to apply it. Both commands accept
`--json`. Profiles require the native lifecycle.

The planner keeps a model when its placement and running recipe match and
Sparky confirms readiness. It stops loaded models outside the profile.
A changed model may require a stop and a start. Sparky reads the capability
groups and their provider lists in file order to start models. Start a model
with a high memory floor before a model that can share the remaining memory.
Sparky checks the
startup floor again as each model starts. If a retained model leaves too
little memory to start another model, the plan can block. It does not
silently evict the retained model.

Apply makes a fresh plan on the controller. It uses the existing lifecycle
and request drain rules. New model requests may wait while the controller
applies changes. A failed switch reports which steps finished and keeps
the last successfully applied profile name. Review the live state and plan
again after a failure. Sparky does not automatically restart the prior set
of models or apply a profile at boot.

List a provider to start it or keep it running. Omit it to stop it.
Use `capabilities: {}` to stop all profile workloads. Profiles
use the installed recipes and do not install services. The gateway,
inference proxy, and Sparky controller run independently of profiles.

Sparky refuses to stop ComfyUI while it has active or queued jobs. Image
workflow requests use the engine selected by the applied profile. The Bifrost
image API requires its configured model node to match the selected engine.
Image checkpoints load per workflow. Profile apply does not change the
image engine memory limits.

Audio containers start in recipe order and stop in reverse order.
Sparky checks Docker health when the container reports it. Sparky checks
the OCR health endpoint and uses the existing OCR controls. Stop operations
can interrupt active audio or OCR requests. Profile apply does not change system boot policies.

The plan reports expected chat and decision auto targets using the current
Bifrost routing policy. Existing route reconciliation uses live model
readiness. `sparky status` reports the last applied profile and actual running
state. Plans show which requested capabilities are available now. Apply
reports availability after it verifies the selected providers.
