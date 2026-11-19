# Cluster profiles

A cluster profile lists the language models, decision models, and services
that should run together. Give each model an explicit placement. The model catalog in
`models.json` still defines the installed recipes and supported placements.

Store profiles in `config/profiles.yaml` beside `models.json` on the head Spark.
For example:

```yaml
version: 1
profiles:
  chat-and-decisions:
    models:
      - glm: both
      - knowline: spark01
    services:
      comfyui: spark02
      kokoro: spark01
      parakeet: spark02
      spark-ocr: spark01
```

Use `sparky profile plan chat-and-decisions` to preview the switch. Use
`sparky profile apply chat-and-decisions` to apply it. Both commands accept
`--json`. Profiles require the native lifecycle.

The planner keeps a model when its placement and running recipe match and
Sparky confirms readiness. It stops loaded models outside the profile.
A changed model may require a stop and a start. The order in `models` is the
startup order for models that need starting. Start a model with a high memory
floor before a model that can share the remaining memory. Sparky checks the
startup floor again as each model starts. If a retained model leaves too
little memory to start another model, the plan can block. It does not
silently evict the retained model.

Apply makes a fresh plan on the controller. It uses the existing lifecycle
and request drain rules. New model requests may wait while the controller
applies changes. A failed switch reports which steps finished and keeps
the last successfully applied profile name. Review the live state and plan
again after a failure. Sparky does not automatically restart the prior set
of models or apply a profile at boot.

`services` controls installed audio services, ComfyUI, and Spark01 OCR.
List a service to start it or keep it running. Omit it to stop it. Profiles
use the installed recipes and do not install services. The gateway,
inference proxy, and Sparky controller run independently of profiles.

Sparky refuses to stop ComfyUI while it has active or queued jobs. Image
requests use the engine selected by the applied profile. They cannot start
an engine the profile leaves stopped. Image checkpoints still load per
workflow. Profile apply does not change the image engine memory limits.

Audio containers start in recipe order and stop in reverse order.
OCR uses the existing OCR controls. Stop operations can interrupt active
audio or OCR requests. Profile apply does not change system boot policies.

The plan reports expected chat and decision auto targets using the current
Bifrost routing policy. Existing route reconciliation uses live model
readiness. `sparky status` reports the last applied profile and actual running
state.
