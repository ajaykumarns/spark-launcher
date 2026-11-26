# Cluster profiles

Model IDs stay the same across profiles. If two workload entries run the same
model with different memory settings, set `public_model` on the alternate entry
to the canonical workload key. For example, `inkling-2-mini-shared` can set
`"public_model": "inkling-2-mini"`. Keep the canonical display name independent
of the device and memory settings.

The catalog exposes one public ID. Inference sends it to the ready runtime in
that group. Old workload IDs remain valid aliases. Requests do not start a
stopped runtime. If multiple runtimes in a group are ready, inference reports
an error instead of choosing an arbitrary target. Profile plans and lifecycle
records keep their internal workload keys so memory and service ownership stay
explicit. OpenCode keeps legacy model-map keys for saved sessions, but those
keys send the canonical API ID.

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
state. Plans show which requested capabilities are available now. Availability
here means the service is running and ready on its node — it is not proof
that the route through the gateway works.

After a successful apply, the CLI sends one tiny OCR request through the
gateway and reports the result under "Route verification". A verified OCR
route means the route reaches the OCR service on its declared node; a failed
check fails the command, so a cron or the menu bar sees a loud failure instead
of an unverified success. Other capabilities (speech, transcription, images)
are reported as "not verified" until a real probe exists for them — a running
service is reported as running, never as verified. `sparky doctor` checks the
same OCR route and exits nonzero when it fails.

`sparky doctor` also checks the per-node service registration with read-only
SSH probes (`tailscale serve status` on the worker node, and the spark_ocr MCP
client's connection string through the gateway admin API on the gateway host).
The serve check derives the service's actual URL from the live serve mapping
— it never hardcodes one — and the MCP check compares the client's connection
string against that URL. An OCR placement change without a gateway re-point
shows up as a failed MCP connection-string check; the same drift shows up as
a failed route probe.

The per-node service-URL catalog lives in
`internal/sparky/service_check.go`: each capability carries its local port,
gateway route, and MCP path. Only OCR has a real probe today; speech and
transcription get the serve-mapping check and report "not verified" in the
route section until a real probe exists for them.

The gateway's per-provider routes (for example the OCR provider's base URL)
and the spark_ocr MCP client connection string are not re-written by Profile
apply or doctor. They are written once when the service is installed, and
drift after an OCR placement change is detected by the route verification and
the service checks above. To re-point either after moving its service, edit
the provider or MCP client through the gateway admin API (the same
base-URL-only pattern the installation uses) on the gateway host, then re-run
`sparky doctor`.

`sparky canary` is the scheduled route check: gateway reachability plus the
OCR route through the gateway. It is silent on success (so a cron delivers
nothing) and prints the failure details and exits nonzero on failure (so a
cron can deliver an alert). It never touches harnesses, services, or SSH —
`sparky doctor` does the full check. The scheduled job is the Mac-local
hermes cron job `route-canary` (no-agent, hourly, failure-deliver
`telegram:8101940290`); the sparky CLI lives on the Mac and the two Spark
devices — not on openclaw, which reaches the gateway only for API access.

During apply, the CLI and menu bar show every active step and its elapsed
time. JSON status includes these steps in `profile_transition.active_steps`.
The saved profile remains the last successful selection until the new profile
passes its final checks.

## Chat routing weights

With `auto.routing: bifrost`, use optional `auto_weights` to set the
`spark-auto` share for each chat
provider in a profile. For example:

```yaml
profiles:
  everyday:
    auto_weights:
      qwen3.8-flash-next: 0.65
      ornith-1.5-35b-a3b-nvfp4: 0.35
    capabilities:
      chat:
        - qwen3.8-flash-next: spark01
        - ornith-1.5-35b-a3b-nvfp4: spark02
```

Each weight must be a finite positive number. Relative values such as 65 and
35 also work. Include every chat provider exactly once. Model aliases are
accepted and stored under their canonical model names. Unknown providers,
decision models, duplicate aliases, and incomplete maps are rejected.

When a weighted profile is selected, `spark-auto` uses only its ready chat
providers that are also permitted by `auto.targets`. Sparky normalizes their
weights. If only one is ready, it receives 100 percent. If none are ready,
Sparky publishes the existing unavailable target. Routing never loads a
stopped model. Models loaded outside a weighted profile remain available
through their direct routes. Profiles without `auto_weights` retain equal
routing across the ready pool.

Bifrost still owns session affinity. The weights choose a route for new
sessions or requests without an affinity match. A stable `x-bf-session-id`
keeps later requests on the chosen model while that affinity remains valid.
The shares are approximate; they do not divide tokens or GPU time.

Profile apply uses the target profile's weights even if no models need a
restart. Normal route reconciliation reads the saved profile file, so
controller restarts and `sparky sync` preserve the configured shares. Invalid
edits report an error and leave the last gateway rule intact. Decision routing
is unchanged. `profiles plan`, `profiles list --json`, and `status --json`
expose the configured or effective shares.
