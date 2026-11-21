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
    description: Chat and decisions with audio, images, and OCR.
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

The YAML key (for example, `everyday`) is the profile ID. Use it in plan
and apply commands. The optional `description` explains the profile in the
CLI and the Switch Profile menu.

Use `sparky profile list` (or `sparky profiles list`) to list profiles,
descriptions, and capabilities. Use
`sparky profile plan everyday` to preview the switch. Use
`sparky profile apply everyday` to apply it. All three commands accept
`--json`. Pass the plan's JSON `plan_id` to
`sparky profile apply everyday --expect-plan ID` to refuse a changed plan.
The menu bar does this when you confirm a switch. Profiles require the
native lifecycle.

The planner keeps a model when its placement and running recipe match and
Sparky confirms readiness. It stops loaded models outside the profile.
A changed model may require a stop and a start. Sparky reads the capability
groups and their provider lists in file order on each device. Start a model
with a high memory floor before a model that can share the remaining memory.
The plan shows which later models Sparky may restart to meet an earlier
model's startup memory floor. Apply checks memory after the planned stops.
If memory is still low, Sparky stops only the listed models it needs to
make room. It starts them again in profile order. Protected models stay
running.

Apply finishes the planned stops before it starts new workloads. Independent
model and service starts on different devices then run in parallel. Steps on
the same device stay in plan order and wait for readiness. A model that uses
both devices waits for earlier steps on both devices. Later steps on either
device wait for that model. Each device can advance without waiting for work
on the other device.

The scheduler also reserves devices used by a conditional peer stop or a
startup memory check. It reads available memory again after earlier steps
and conditional stops finish. It enforces configured startup memory floors
for models and services. The native model supervisor retains its own memory
and image-job checks. The final memory check runs after device admission.
Service starts also hold an exclusive device reservation through
readiness. Plans include service startup memory floors and block starts
that cannot fit without a planned memory change on that device.
Conditional peer stops keep their gateway update and
shutdown in sequence. Final route updates run after all starts finish.

Apply makes a fresh plan on the controller. It uses the existing lifecycle
and request drain rules. New model requests may wait while the controller
applies changes. A failed switch reports which steps finished and keeps
the last successfully applied profile name. Review the live state and plan
again after a failure. Sparky does not automatically restart the prior set
of models or apply a profile at boot. After a start fails, Sparky schedules no
new starts. It waits for starts already in progress to finish and includes
their completed steps and all startup errors in the failure result. Failed
CLI results list completed steps instead of presenting unattempted plan
steps as completed work.

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

During apply, the CLI and menu bar show every active step and its elapsed
time. JSON status includes these steps in `profile_transition.active_steps`.
The saved profile remains the last successful selection until the new profile
passes its final checks.
