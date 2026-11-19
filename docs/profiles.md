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
      parakeet: spark02
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

`services` controls Parakeet. Include it to start its installed containers or
keep them running. Omit it to stop the containers. The profile stops the
gateway before the engine and starts the engine before the gateway. Profile
apply can stop Parakeet even when the catalog protects it from direct unload.
Sparky gives containers 30 seconds to stop. Audio requests do not use the
language model drain mechanism.

`observe` lists services that must already be running. It checks Kokoro,
ComfyUI, and Spark01 OCR. These services keep their existing lifecycle
controls. The plan reports
expected chat and decision auto targets using the current Bifrost routing
policy. Existing route reconciliation uses live model readiness.
`sparky status` reports the last applied profile and the actual running state.
