// Deployment foundation: private file transfer through the Go node helper,
// unit installation, bounded readiness and idle waits, and the inactive Qwen
// dual recipe preparation. No model is ever started, no container is touched,
// and no memory guard is disabled; the existing Python engines stay the
// authority for engine-owned scripts. All effects are faked in tests.
package sparky

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// deploymentRun is the injectable command seam; tests never invoke ssh or
// sudo. Diagnostics never carry a host name, URL, or command output.
var deploymentRun = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
	return runCommand(ctx, args, input)
}

// httpWaitPoll and comfyIdlePoll are the wait cadences. They are package
// variables so tests can shrink the real clocks; production limits are set
// here and never mutated.
var (
	httpWaitPoll  = time.Second
	comfyIdlePoll = time.Second
)

// waitHTTP polls a readiness URL until it answers, bounded by the timeout
// and the context. A transient failure is retried, never read as ready.
func waitHTTP(ctx context.Context, url string, timeout time.Duration) (Object, error) {
	deadline := time.Now().Add(timeout)
	for {
		if o, err := requestObject(ctx, "GET", url, nil, nil, 3*time.Second, false); err == nil {
			return o, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fail("Service readiness timed out.")
		}
		select {
		case <-ctx.Done():
			return nil, fail("Service readiness was cancelled.")
		case <-time.After(httpWaitPoll):
		}
	}
}

// waitComfyIdle requires three consecutive confirmed idle /queue arrays.
// A missing, null, or wrong-type queue field and any HTTP failure reset the
// count: idle is only ever a confirmed observation, never an inference from
// failure. A timeout of zero or less waits unboundedly on the context, which
// is the ExecStop drain semantic that preserves queued jobs.
func waitComfyIdle(ctx context.Context, url string, timeout time.Duration) error {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	quiet := 0
	for {
		idle := false
		if data, err := requestObject(ctx, "GET", url+"/queue", nil, nil, 10*time.Second, false); err == nil {
			running, okRunning := data["queue_running"].([]any)
			pending, okPending := data["queue_pending"].([]any)
			idle = okRunning && okPending && len(running) == 0 && len(pending) == 0
		}
		if idle {
			quiet++
		} else {
			quiet = 0
		}
		if quiet == 3 {
			return nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return fail("ComfyUI did not confirm idle in time.")
		}
		select {
		case <-ctx.Done():
			return fail("The ComfyUI drain was cancelled.")
		case <-time.After(comfyIdlePoll):
		}
	}
}

// drainComfy asks the Sparky drain endpoint to stop admitting new work and
// then confirms the queue is empty. The drain request is best effort: an
// engine without the endpoint still drains through the queue confirmation,
// and its failure is never read as idle. Nothing is ever cancelled or
// interrupted; queued jobs keep running to completion.
func drainComfy(ctx context.Context, file string) error {
	node, err := readObject(file)
	if err != nil {
		return fail("Could not read the node file.")
	}
	url := strings.TrimSuffix(str(node["comfy_url"]), "/")
	if url == "" {
		return fail("The node file has no ComfyUI address.")
	}
	_, _ = requestObject(ctx, "POST", url+"/sparky/drain", Object{}, nil, 5*time.Second, false)
	return waitComfyIdle(ctx, url, 0)
}

// DrainComfy is the unbounded ExecStop drain: it preserves queued jobs for
// their whole lifetime and never forces cancellation. Supervisors and tests
// use drainComfy with their own context instead.
func DrainComfy(file string) error {
	return drainComfy(context.Background(), file)
}

// runtimeActivityIdle waits until the runtime reports no active manager
// requests. A query failure is an error, never idle. Callers bound the wait
// with their own context; the installer's supervisor passes an unbounded one
// so old requests that predate the change keep running untouched.
func runtimeActivityIdle(ctx context.Context, rt *Runtime) error {
	for {
		activity, err := rt.legacyActivity(ctx)
		if err != nil {
			return err
		}
		busy := false
		for _, count := range activity {
			if count > 0 {
				busy = true
			}
		}
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return fail("The runtime activity wait was cancelled.")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// qwenDualFixture is the exact expected source layout in the cluster extra
// directory. Any drift is rejected before a single write.
const qwenRankCheck = "if model.startswith(\"qwen\") and rank != 0:\n    raise SystemExit(\"This Qwen recipe uses spark01 only\")\n"
const qwenLauncherBlock = "if [[ \"$model\" == qwen3.8-flash-next && \"$rank\" != 0 ]]; then\n echo 'This Qwen recipe uses spark01 only' >&2\n exit 2\nfi\n"
const qwenMemoryBlock = "if [[ \"$model\" == qwen3.8-flash-next ]]; then\n python3 \"$base/extra/prepare-qwen-memory.py\"\nfi\n"

// qwenDualCounted asserts one exact occurrence; anything else is a changed
// layout and aborts with zero effects.
func qwenDualCounted(text, fragment, what string) error {
	if strings.Count(text, fragment) != 1 {
		return fail("Unexpected %s layout. No dual recipe was installed.", what)
	}
	return nil
}

// journalServiceUser validates a systemd User= name: a bare token with no
// whitespace, control character, quote, or backslash; it is written
// unquoted because User= parses no quoting or specifiers.
func journalServiceUser(name string) (string, error) {
	if name == "" {
		return "", fail("The service user must not be empty.")
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || r == '"' || r == '\\' {
			return "", fail("The service user name is not valid.")
		}
	}
	return name, nil
}

// prepareQwenDual runs the transactional preparation with its own journal:
// a failure rolls back every recorded change, success commits.
func prepareQwenDual(ctx context.Context, cfg Object, stage string) error {
	journal := NewDeploymentJournal(cfg, stage)
	if err := prepareQwenDualWithJournal(ctx, cfg, stage, journal); err != nil {
		if rberr := journal.Rollback(); rberr != nil {
			return fail("%v %v", err, rberr)
		}
		return err
	}
	return nil
}

// prepareQwenDualWithJournal derives the two-rank Qwen variant from the
// installed recipe without starting, replacing, or reloading any engine,
// recording every change in the caller's journal so a parent transaction
// (InstallHead) can roll the whole deployment back. The source layouts and
// flag counts are validated in full before the first write. Quantization,
// prefix-caching, and MTP flags are kept untouched; the unit carries
// Restart=no and is never enabled.
func prepareQwenDualWithJournal(ctx context.Context, cfg Object, stage string, journal *DeploymentJournal) error {
	extra := filepath.Join(str(cfg["root"]), "extra")
	recipeRaw, err := os.ReadFile(filepath.Join(extra, "nvidia-recipes", "qwen3.8-flash-next.json"))
	if err != nil {
		return fail("The installed NVIDIA recipe is missing.")
	}
	recipe, err := decode(recipeRaw)
	if err != nil {
		return fail("The installed NVIDIA recipe is not a valid JSON object.")
	}
	serve := obj(recipe["serve"])
	base := str(serve["command"])
	if base == "" {
		return fail("The installed NVIDIA recipe has no serve command.")
	}
	// The three pinned rewrites, each required exactly once.
	for _, rewrite := range []struct {
		flag  string
		value float64
	}{
		{"--tensor-parallel-size", 2},
		{"--max-num-seqs", 4},
		{"--gpu-memory-utilization", 0.75},
	} {
		var count int
		base, count, err = recipeReplaceFlag(base, rewrite.flag, rewrite.value, "command")
		if err != nil || count != 1 {
			return fail("The installed NVIDIA recipe does not have one %s setting. No dual recipe was installed.", rewrite.flag)
		}
	}
	sourceRaw, err := os.ReadFile(filepath.Join(extra, "serve-nvidia.py"))
	if err != nil {
		return fail("The installed NVIDIA engine script is missing.")
	}
	source := string(sourceRaw)
	for fragment, what := range map[string]string{
		qwenRankCheck:            "engine script",
		`(model + ".json")`:      "engine script",
		`env = dict(os.environ)`: "engine script",
	} {
		if err := qwenDualCounted(source, fragment, what); err != nil {
			return err
		}
	}
	launcherRaw, err := os.ReadFile(filepath.Join(extra, "start-local-nvidia.sh"))
	if err != nil {
		return fail("The installed NVIDIA launcher is missing.")
	}
	launcher := string(launcherRaw)
	for fragment, what := range map[string]string{
		qwenLauncherBlock: "launcher",
		qwenMemoryBlock:   "launcher",
		`cache="$base/extra/cache/$model-fp4-local"`: "launcher",
		"/service/serve-nvidia.py":                   "launcher",
	} {
		if err := qwenDualCounted(launcher, fragment, what); err != nil {
			return err
		}
	}
	// All layouts are confirmed: derive the payloads.
	base += " \\\n  --nnodes 2 --node-rank 0 --master-addr \"$HEAD_IP\" --master-port \"$MASTER_PORT\""
	serve["command"] = base
	worker := strings.ReplaceAll(base, "--node-rank 0", "--node-rank 1") + " \\\n  --headless"
	serve["additional_commands"] = []any{Object{"id": "worker", "command": worker}}
	source = strings.ReplaceAll(source, qwenRankCheck, "")
	source = strings.ReplaceAll(source, `(model + ".json")`, `(model + "-dual.json")`)
	source = strings.ReplaceAll(source, `env = dict(os.environ)`,
		"env = dict(os.environ)\nenv[\"B12X_COMPILE_WORKERS\"] = \"1\"\nenv[\"PYTORCH_CUDA_ALLOC_CONF\"] = \"expandable_segments:True\"")
	launcher = strings.ReplaceAll(launcher, qwenLauncherBlock, "")
	launcher = strings.ReplaceAll(launcher, qwenMemoryBlock, "")
	launcher = strings.ReplaceAll(launcher, `cache="$base/extra/cache/$model-fp4-local"`, `cache="$base/extra/cache/$model-fp4-local-dual"`)
	launcher = strings.ReplaceAll(launcher, "/service/serve-nvidia.py", "/service/serve-qwen-dual.py")
	// The unit runs as the installing user's name, mirroring the source.
	user, err := journalServiceUser(filepath.Base(home()))
	if err != nil {
		return err
	}
	script, err := systemdExecValue(filepath.Join(extra, "start-qwen-dual.sh"))
	if err != nil {
		return err
	}
	unit := "[Unit]\n" +
		"Description=Sparky Qwen two-device recipe\n" +
		"After=docker.service network-online.target\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"User=" + user + "\n" +
		"ExecStart=:" + script + " qwen3.8-flash-next\n" +
		"TimeoutStartSec=1800\n" +
		"TimeoutStopSec=180\n" +
		"KillMode=control-group\n" +
		"Restart=no\n"
	recipeBytes := jsonBytes(recipe)
	payloads := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{filepath.Join(extra, "nvidia-recipes", "qwen3.8-flash-next-dual.json"), recipeBytes, 0o600},
		{filepath.Join(extra, "serve-qwen-dual.py"), []byte(source), 0o644},
		{filepath.Join(extra, "start-qwen-dual.sh"), []byte(launcher), 0o755},
	}
	for _, payload := range payloads {
		for _, node := range []string{"spark01", "spark02"} {
			if err := journal.WriteFile(ctx, node, payload.path, payload.data, payload.mode); err != nil {
				return err
			}
		}
	}
	for _, node := range []string{"spark01", "spark02"} {
		if err := journal.WriteUnit(ctx, node, "/etc/systemd/system/sparky-qwen-dual.service", unit); err != nil {
			return err
		}
	}
	return nil
}
