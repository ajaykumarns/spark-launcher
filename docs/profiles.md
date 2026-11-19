# Cluster profiles

A cluster profile lists the language and decision models that should run
together. Give each model an explicit placement. The model catalog in
`models.json` still defines the installed recipes and supported placements.

Store profiles in `config/profiles.json` beside `models.json` on the head Spark.
For example:

```json
{
  "version": 1,
  "profiles": {
    "chat-and-decisions": {
      "models": [
        {"model": "glm", "on": "both"},
        {"model": "knowline", "on": "spark01"}
      ],
      "observe": {"parakeet": "spark02"}
    }
  }
}
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

`observe` lists services that must already be running. This version checks
audio, ComfyUI, and Spark01 OCR without starting or stopping them. Those
services still use their existing lifecycle controls. The plan reports
expected chat and decision auto targets using the current Bifrost routing
policy. Existing route reconciliation uses live model readiness.
`sparky status` reports the last applied profile and the actual running state.
