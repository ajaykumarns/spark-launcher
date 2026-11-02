package sparky

// Image API and native wire tests. All effects are local: the ComfyUI
// engine and the language-model backend are httptest servers, the command
// runner answers ssh probes and systemctl state queries exactly, and the
// cluster config and lifecycle records live in a temp directory. Seam
// variables are always restored with defer, so no test waits on a real
// clock or leaks timing into another test.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// imageEngine is a local ComfyUI stand-in: /prompt, /history/<id>, /view,
// /health, /sparky/state, /queue and /sparky/free. Every other request is
// recorded as unexpected and refused, so accidental engine calls fail
// loudly instead of being silently tolerated.
type imageEngine struct {
	mu           sync.Mutex
	server       *httptest.Server
	promptID     string
	promptBodies []Object
	history      string // raw /history response; "" means an empty object
	histHits     int
	viewHits     int
	viewBody     []byte
	viewQuery    string
	release      chan struct{} // when non-nil, /history blocks until closed
	unexpected   []string
}

// TestMain shrinks the image seams to test budgets: every test runs against
// real local httptest engines, so no test ever waits on a production clock.
// Production limits are never touched.
func TestMain(m *testing.M) {
	imageGateWait = 5 * time.Second
	imagePollBudget, imagePollInterval = 5*time.Second, 2*time.Millisecond
	imageHistoryTimeout, imageViewTimeout, imageWireBudget = 2*time.Second, 2*time.Second, 2*time.Second
	os.Exit(m.Run())
}

func newImageEngine(t *testing.T) *imageEngine {
	e := &imageEngine{promptID: "job-1", viewBody: append(append([]byte{}, pngSignature...), []byte("fake-png")...)}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/prompt":
			dec := json.NewDecoder(r.Body)
			dec.UseNumber()
			body := Object{}
			if err := dec.Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			e.mu.Lock()
			e.promptBodies = append(e.promptBodies, body)
			e.mu.Unlock()
			_, _ = w.Write([]byte(`{"prompt_id":` + quoteJSON(e.promptID) + `}`))
		case strings.HasPrefix(r.URL.Path, "/history/"):
			e.mu.Lock()
			e.histHits++
			e.mu.Unlock()
			if release := e.releaseChannel(); release != nil {
				<-release
			}
			e.mu.Lock()
			raw := e.history
			e.mu.Unlock()
			if raw == "" {
				raw = "{}"
			}
			_, _ = w.Write([]byte(raw))
		case r.URL.Path == "/view":
			e.mu.Lock()
			e.viewHits++
			e.viewQuery = r.URL.RawQuery
			e.mu.Unlock()
			_, _ = w.Write(e.viewBody)
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/sparky/state":
			_, _ = w.Write([]byte(`{"executing":false,"prompt_id":null,"phase":"idle"}`))
		case r.URL.Path == "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[]}`))
		case r.URL.Path == "/sparky/free":
			_, _ = w.Write([]byte(`{}`))
		default:
			e.mu.Lock()
			e.unexpected = append(e.unexpected, r.Method+" "+r.URL.Path)
			e.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(e.server.Close)
	return e
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (e *imageEngine) releaseChannel() chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.release
}

func (e *imageEngine) prompts() []Object {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Object{}, e.promptBodies...)
}

func (e *imageEngine) setHistory(raw string) {
	e.mu.Lock()
	e.history = raw
	e.mu.Unlock()
}

func (e *imageEngine) setPromptID(id string) {
	e.mu.Lock()
	e.promptID = id
	e.mu.Unlock()
}

func (e *imageEngine) noUnexpected(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.unexpected) > 0 {
		t.Fatalf("unexpected engine requests: %v", e.unexpected)
	}
}

// completedHistory is the accepted shape of a finished ComfyUI job, keyed by
// the engine's actual prompt id.
func completedHistory(id, status string) string {
	return fmt.Sprintf(`{%q:{"status":{"completed":true,"status_str":%s},"outputs":{"13":{"images":[{"filename":"Sparky-FLUX2-Klein_00001_.png","subfolder":"","type":"output"}]}}}}`, id, quoteJSON(status))
}

// imageLLM is the language-model backend: health plus chat completions.
type imageLLM struct {
	server  *httptest.Server
	mu      sync.Mutex
	bodies  []Object
	request string
}

func newImageLLM(t *testing.T) *imageLLM {
	l := &imageLLM{}
	l.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			// The backend serves exactly the canonical key "glm" for glm.
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm","object":"model"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			dec := json.NewDecoder(r.Body)
			dec.UseNumber()
			body := Object{}
			_ = dec.Decode(&body)
			l.mu.Lock()
			l.bodies = append(l.bodies, body)
			l.mu.Unlock()
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		default:
			l.mu.Lock()
			l.request = r.Method + " " + r.URL.Path
			l.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(l.server.Close)
	return l
}

// imageConfig builds a valid native cluster with one LLM, the image model
// and ComfyUI, all pointed at the local test servers.
func imageConfig(root, engineURL, llmURL string) Object {
	return Object{
		"version": 1, "root": root, "controller_port": 8020, "lifecycle": "native",
		"nodes": Object{
			"spark01": Object{"direct_ip": "10.0.0.1"},
			"spark02": Object{"direct_ip": "10.0.0.2", "ssh": "spark02", "comfy_url": engineURL, "comfy_unit": "comfyui.service"},
		},
		"policy": Object{"interrupt_active_jobs": false, "protected_models": []any{},
			"comfyui_exclusive_with_llms": false,
			"comfyui_shared_llms":         []any{"glm"},
			"comfyui_min_available_gib":   18},
		"auto": Object{"targets": []any{"glm"}, "default": "glm"},
		"models": Object{
			"glm": Object{"kind": "llm", "name": "GLM 5.3 Flash", "run_on": "spark01", "keep_loaded": false, "reasoning": false, "aliases": []any{},
				"settings": Object{"spark01": Object{"backend_url": llmURL, "units": Object{"spark01": "glm.service"},
					"context_tokens": 262144, "concurrency": 4}}},
			"flux2-klein-4b": Object{"kind": "comfy_model", "name": "FLUX.2 Klein 4B", "run_on": "spark02",
				"keep_loaded": false, "reasoning": false, "aliases": []any{},
				"settings": Object{"spark02": Object{}}, "image_api": Object{"enabled": true}},
			"disabled-image": Object{"kind": "comfy_model", "name": "Disabled", "run_on": "spark02",
				"keep_loaded": false, "reasoning": false, "aliases": []any{},
				"settings": Object{"spark02": Object{}}, "image_api": Object{"enabled": false}},
			"comfyui": Object{"kind": "comfyui", "name": "ComfyUI", "run_on": "spark02",
				"keep_loaded": false, "reasoning": false, "aliases": []any{},
				"settings": Object{"spark02": Object{"backend_url": engineURL, "units": Object{"spark02": "comfyui.service"},
					"context_tokens": 1, "concurrency": 1}}},
		},
	}
}

type imageEnv struct {
	t       *testing.T
	coord   *Coordinator
	rt      *Runtime
	dev     *fakeDevice
	engine  *imageEngine
	llm     *imageLLM
	path    string
	root    string
	runnerC *runnerCalls
}

// runnerCalls records every command so tests can assert that no real
// lifecycle effect (start, stop, restart) ever happened.
type runnerCalls struct {
	mu    sync.Mutex
	argvs [][]string
}

func (c *runnerCalls) record(args []string) {
	c.mu.Lock()
	c.argvs = append(c.argvs, args)
	c.mu.Unlock()
}

func (c *runnerCalls) all() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]string{}, c.argvs...)
}

// imageRunner answers the ssh node probe and the supervisor state query.
// Anything else is a loud failure: image tests must never start units.
func imageRunner(dev *fakeDevice, calls *runnerCalls) Runner {
	return func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		calls.record(args)
		switch args[0] {
		case "ssh":
			if strings.Contains(args[len(args)-1], "is-active") {
				return CommandResult{Code: 0}, nil
			}
			probe, _ := dev.probe(nil)
			return CommandResult{Out: string(jsonBytes(probe))}, nil
		case "systemctl":
			if len(args) > 1 && args[1] == "show" {
				return CommandResult{Out: "active\n", Code: 0}, nil
			}
			if len(args) > 1 && args[1] == "is-active" {
				// Only the glm rank unit is active; anything else stays inactive.
				if args[len(args)-1] == "glm.service" {
					return CommandResult{Code: 0}, nil
				}
				return CommandResult{Code: 3}, nil
			}
			return CommandResult{Out: "inactive\n", Code: 0}, nil
		}
		return CommandResult{}, fmt.Errorf("unexpected command in image test: %v", args)
	}
}

func newImageEnv(t *testing.T, lifecycle string) *imageEnv {
	t.Helper()
	root := t.TempDir()
	engine := newImageEngine(t)
	llm := newImageLLM(t)
	cfg := imageConfig(root, engine.server.URL, llm.server.URL)
	if lifecycle != "native" {
		cfg["lifecycle"] = lifecycle
		cfg["manager_url"] = "http://127.0.0.1:9"
	}
	path := filepath.Join(root, "models.json")
	if err := writeCluster(path, cfg); err != nil {
		t.Fatal(err)
	}
	dev := &fakeDevice{Mem: 23 * gib, Units: map[string]string{}, Reachable: true}
	calls := &runnerCalls{}
	rt := newRuntime(cfg, path)
	rt.Run = imageRunner(dev, calls)
	rt.ProbeNode = dev.probe
	coord, err := newCoordinator(path, rt)
	if err != nil {
		t.Fatal(err)
	}
	return &imageEnv{t: t, coord: coord, rt: rt, dev: dev, engine: engine, llm: llm, path: path, root: root, runnerC: calls}
}

// readyRecord writes the lifecycle record that makes Runtime.ready return
// true for glm on spark01: a fully validated READY record with the recipe
// fingerprint, a durable generation and the exact served model id.
func (e *imageEnv) readyRecord() {
	e.t.Helper()
	cfg := e.coord.Config()
	m := obj(obj(cfg["models"])["glm"])
	setting := obj(obj(m["settings"])["spark01"])
	rec, err := e.rt.BeginOperation("glm", m, setting, []string{"spark01"}, "spark01")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.rt.TransitionState("glm", str(rec["generation"]), StateStarting); err != nil {
		e.t.Fatal(err)
	}
	if err := e.rt.TransitionState("glm", str(rec["generation"]), StateReady); err != nil {
		e.t.Fatal(err)
	}
}

func validImageBody() Object {
	return Object{"model": "flux2-klein-4b", "prompt": "a small robot watering a plant"}
}

func (e *imageEnv) generate(body Object) (Object, error) {
	return e.coord.Generate(body)
}

func assertStatus(t *testing.T, err error, want int, substr string) {
	t.Helper()
	if err == nil {
		t.Fatal("an error was expected")
	}
	var se statusError
	if !errors.As(err, &se) || se.HTTPStatus() != want {
		t.Fatalf("expected HTTP %d, got %v", want, err)
	}
	if substr != "" && !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error mentioning %q, got %v", substr, err)
	}
}

func TestImageValidationBeforeSubmit(t *testing.T) {
	env := newImageEnv(t, "native")
	cases := []struct {
		name string
		body Object
		msg  string
	}{
		{"unknown field", Object{"model": "flux2-klein-4b", "prompt": "x", "temperature": 1}, "Unsupported image fields"},
		{"blank prompt", Object{"model": "flux2-klein-4b", "prompt": "   "}, "1 to 8000"},
		{"missing prompt", Object{"model": "flux2-klein-4b"}, "1 to 8000"},
		{"too long prompt", Object{"model": "flux2-klein-4b", "prompt": strings.Repeat("水", 8001)}, "1 to 8000"},
		{"n too large", Object{"model": "flux2-klein-4b", "prompt": "x", "n": 8}, "one image"},
		{"n fractional", Object{"model": "flux2-klein-4b", "prompt": "x", "n": json.Number("1.5")}, "one image"},
		{"n boolean", Object{"model": "flux2-klein-4b", "prompt": "x", "n": true}, "one image"},
		{"streaming", Object{"model": "flux2-klein-4b", "prompt": "x", "stream": true}, "streaming"},
		{"bad quality", Object{"model": "flux2-klein-4b", "prompt": "x", "quality": "high"}, "auto or standard"},
		{"url response", Object{"model": "flux2-klein-4b", "prompt": "x", "response_format": "url"}, "b64_json"},
		{"bad size", Object{"model": "flux2-klein-4b", "prompt": "x", "size": "800x600"}, "512x512"},
		{"non string size", Object{"model": "flux2-klein-4b", "prompt": "x", "size": 512}, "512x512"},
		{"boolean seed", Object{"model": "flux2-klein-4b", "prompt": "x", "seed": true}, "unsigned 64-bit"},
		{"negative seed", Object{"model": "flux2-klein-4b", "prompt": "x", "seed": -1}, "unsigned 64-bit"},
		{"fractional seed", Object{"model": "flux2-klein-4b", "prompt": "x", "seed": json.Number("1.5")}, "unsigned 64-bit"},
		{"overflow seed", Object{"model": "flux2-klein-4b", "prompt": "x", "seed": json.Number("18446744073709551616")}, "unsigned 64-bit"},
		{"float seed", Object{"model": "flux2-klein-4b", "prompt": "x", "seed": float64(42)}, "unsigned 64-bit"},
		{"unknown model", Object{"model": "gpt-image-1", "prompt": "x"}, "Unknown model"},
		{"non image model", Object{"model": "glm", "prompt": "x"}, "no tested gateway workflow"},
	}
	for _, tc := range cases {
		_, err := env.generate(tc.body)
		if err == nil {
			t.Fatalf("%s: an error was expected", tc.name)
		}
		var se statusError
		if !errors.As(err, &se) || se.HTTPStatus() != 400 {
			t.Fatalf("%s: expected HTTP 400, got %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.msg) {
			t.Fatalf("%s: expected %q in %v", tc.name, tc.msg, err)
		}
	}
	if prompts := env.engine.prompts(); len(prompts) != 0 {
		t.Fatalf("invalid input must never reach the engine: %d submissions", len(prompts))
	}
	if calls := env.runnerC.all(); len(calls) != 0 {
		t.Fatalf("validation must not touch devices: %v", calls)
	}
}

func TestImageSeedExactPreserved(t *testing.T) {
	env := newImageEnv(t, "native")
	env.engine.setHistory(completedHistory("job-1", "success"))
	body := validImageBody()
	body["seed"] = json.Number("18446744073709551615")
	if _, err := env.generate(body); err != nil {
		t.Fatal(err)
	}
	prompts := env.engine.prompts()
	if len(prompts) != 1 {
		t.Fatalf("exactly one submission expected: %d", len(prompts))
	}
	workflow := obj(prompts[0]["prompt"])
	seed := obj(obj(workflow["7"])["inputs"])["noise_seed"]
	if n, ok := seed.(json.Number); !ok || n.String() != "18446744073709551615" {
		t.Fatalf("the exact seed must survive into the workflow, got %#v", seed)
	}
	// The exact tested graph shape is submitted, nothing else.
	if got := str(obj(obj(workflow["1"])["inputs"])["unet_name"]); got != "flux-2-klein-4b-fp8.safetensors" {
		t.Fatalf("wrong UNET: %q", got)
	}
	if got := str(obj(obj(workflow["13"])["inputs"])["filename_prefix"]); got != "Sparky-FLUX2-Klein" {
		t.Fatalf("wrong output prefix: %q", got)
	}
	if len(workflow) != 13 {
		t.Fatalf("the workflow has exactly 13 nodes, got %d", len(workflow))
	}
}

func TestImageHappyPathAndShape(t *testing.T) {
	env := newImageEnv(t, "native")
	env.engine.setHistory(completedHistory("job-1", "success"))
	result, err := env.generate(validImageBody())
	if err != nil {
		t.Fatal(err)
	}
	data := arr(result["data"])
	if len(data) != 1 {
		t.Fatalf("exactly one image expected: %v", result)
	}
	encoded := str(obj(data[0])["b64_json"])
	if !strings.HasPrefix(encoded, "iVBOR") {
		t.Fatalf("the PNG must be base64 encoded, got %.10q", encoded)
	}
	if result["created"] == nil {
		t.Fatal("a created timestamp is required")
	}
	// Events carry metadata only, never the prompt.
	events, err := os.ReadFile(filepath.Join(env.root, "controller", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(events), "\n") {
		if strings.Contains(line, "image_api_completed") {
			if strings.Contains(line, "a small robot") {
				t.Fatal("the prompt must never be recorded in events")
			}
			row := obj(mustDecode(t, line))
			if str(row["workload"]) != "flux2-klein-4b" || str(row["node"]) != "spark02" || str(row["prompt_id"]) != "job-1" {
				t.Fatalf("wrong event metadata: %v", row)
			}
		}
	}
	// The gate is released: a second request goes through, polled on its own
	// accepted job's history entry.
	env.engine.setPromptID("job-2")
	env.engine.setHistory(completedHistory("job-2", "success"))
	if _, err := env.generate(validImageBody()); err != nil {
		t.Fatalf("the gate must be released after success: %v", err)
	}
}

func mustDecode(t *testing.T, line string) Object {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	o := Object{}
	if err := dec.Decode(&o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestImageEngineFailureKeepsJob(t *testing.T) {
	env := newImageEnv(t, "native")
	env.engine.setHistory(completedHistory("job-1", "error"))
	_, err := env.generate(validImageBody())
	assertStatus(t, err, 409, "failed")
	// Nothing was ever cancelled or interrupted.
	for _, args := range env.runnerC.all() {
		if args[0] == "sudo" {
			t.Fatalf("an engine failure must never cancel the accepted job: %v", args)
		}
	}
	env.engine.noUnexpected(t)
}

func TestImageTimeoutKeepsAcceptedJob(t *testing.T) {
	env := newImageEnv(t, "native")
	prevBudget, prevInterval := imagePollBudget, imagePollInterval
	imagePollBudget, imagePollInterval = 50*time.Millisecond, 5*time.Millisecond
	defer func() { imagePollBudget, imagePollInterval = prevBudget, prevInterval }()
	_, err := env.generate(validImageBody())
	assertStatus(t, err, 409, "kept")
	if prompts := env.engine.prompts(); len(prompts) != 1 {
		t.Fatalf("the job was accepted once: %d", len(prompts))
	}
	for _, args := range env.runnerC.all() {
		if args[0] == "sudo" {
			t.Fatalf("a timeout must never cancel the accepted job: %v", args)
		}
	}
	// The gate is released so a later request is not blocked forever.
	env.engine.setHistory(completedHistory("job-1", "success"))
	if _, err := env.generate(validImageBody()); err != nil {
		t.Fatalf("the gate must be released after a timeout: %v", err)
	}
}

func TestImageGateBusy(t *testing.T) {
	env := newImageEnv(t, "native")
	prevWait := imageGateWait
	imageGateWait = 100 * time.Millisecond
	defer func() { imageGateWait = prevWait }()
	env.coord.imageGate <- struct{}{}
	_, err := env.generate(validImageBody())
	assertStatus(t, err, 409, "busy")
	<-env.coord.imageGate
	if prompts := env.engine.prompts(); len(prompts) != 0 {
		t.Fatal("a busy gate must not submit anything")
	}
}

func TestImageConcurrentGate(t *testing.T) {
	env := newImageEnv(t, "native")
	// The gate wait is shrunk so the busy probe does not outlast the held
	// request's own poll budget.
	prevWait := imageGateWait
	imageGateWait = 300 * time.Millisecond
	defer func() { imageGateWait = prevWait }()
	held := make(chan struct{})
	env.engine.mu.Lock()
	env.engine.release = held
	env.engine.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := env.generate(validImageBody())
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(env.engine.prompts()) == 0 {
		time.Sleep(time.Millisecond)
	}
	if len(env.engine.prompts()) != 1 {
		t.Fatal("the first request was never submitted")
	}
	_, err := env.generate(validImageBody())
	assertStatus(t, err, 409, "busy")
	env.engine.setHistory(completedHistory("job-1", "success"))
	close(held)
	if err := <-done; err != nil {
		t.Fatalf("the held request must succeed: %v", err)
	}
	if _, err := env.generate(validImageBody()); err != nil {
		t.Fatalf("a later request must be admitted: %v", err)
	}
}

func TestImageOutputValidation(t *testing.T) {
	env := newImageEnv(t, "native")
	// A wrong signature is refused.
	env.engine.viewBody = []byte("not a png at all")
	env.engine.setHistory(completedHistory("job-1", "success"))
	if _, err := env.generate(validImageBody()); err == nil {
		t.Fatal("a non-PNG view response must fail")
	}
	// An oversize PNG is refused by length, never truncated.
	env.engine.viewBody = append(append([]byte{}, pngSignature...), make([]byte, imageMaxPNG)...)
	if _, err := env.generate(validImageBody()); err == nil {
		t.Fatal("an oversize PNG must fail")
	}
	env.engine.noUnexpected(t)
}

func TestImageModelsList(t *testing.T) {
	env := newImageEnv(t, "native")
	rows := env.coord.ImageModels()
	if len(rows) != 1 {
		t.Fatalf("only the enabled image model is listed: %v", rows)
	}
	row := obj(rows[0])
	if str(row["id"]) != "flux2-klein-4b" || str(row["object"]) != "model" || str(row["owned_by"]) != "garuda" {
		t.Fatalf("wrong image model row: %v", row)
	}
	if !contains(stringsOf(row["supported_endpoints"]), "/v1/images/generations") {
		t.Fatalf("the endpoint must be advertised: %v", row)
	}
	if s := stringsOf(row["output_modalities"]); len(s) != 1 || s[0] != "image" {
		t.Fatalf("wrong modalities: %v", row)
	}
}

func TestLLMWireNativeReadiness(t *testing.T) {
	env := newImageEnv(t, "native")
	// Unloaded: no lifecycle records exist yet.
	wire, err := env.coord.LLMWire()
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]Object{}
	for _, r := range arr(wire["data"]) {
		row := obj(r)
		rows[str(row["id"])] = row
		if _, has := row["backend_url"]; has {
			t.Fatal("backend URLs must never be exposed in the listing")
		}
		if str(row["id"]) != "spark-auto" && str(obj(row["status"])["value"]) != "unloaded" {
			t.Fatalf("without records everything is unloaded: %v", row)
		}
	}
	if _, ok := rows["glm"]; !ok {
		t.Fatal("glm is missing")
	}
	if _, ok := rows["spark-auto"]; !ok {
		t.Fatal("spark-auto is missing")
	}
	// No garuda-prefixed catalog rows leak into the wire listing.
	for id := range rows {
		if strings.HasPrefix(id, "garuda/") {
			t.Fatalf("wire ids are unprefixed: %v", id)
		}
	}
	// With a ready record and an active supervisor, glm and spark-auto are loaded.
	env.readyRecord()
	wire, err = env.coord.LLMWire()
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, r := range arr(wire["data"]) {
		row := obj(r)
		statuses[str(row["id"])] = str(obj(row["status"])["value"])
	}
	if statuses["glm"] != "loaded" || statuses["spark-auto"] != "loaded" {
		t.Fatalf("expected loaded glm and spark-auto, got %v", statuses)
	}
}

func TestLLMWireLegacyPreservesManagerErrors(t *testing.T) {
	env := newImageEnv(t, "legacy")
	_, err := env.coord.LLMWire()
	if err == nil || !strings.Contains(err.Error(), "Private manager credential is missing.") {
		t.Fatalf("the manager error must be preserved verbatim: %v", err)
	}
}

func TestImageHTTPIntegration(t *testing.T) {
	env := newImageEnv(t, "native")
	env.readyRecord()
	env.engine.setHistory(completedHistory("job-1", "success"))
	s := &controllerServer{ctrl: env.coord}

	// The full model listing: native wire rows with catalog overlay plus one
	// image row, no credentials anywhere.
	rec := serverRequest(t, s, "GET", "/v1/models", "127.0.0.1:5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("listing failed: %d %s", rec.Code, rec.Body.String())
	}
	wire := decodeResponse(t, rec.Body.String())
	seen := map[string]int{}
	for _, r := range arr(wire["data"]) {
		row := obj(r)
		id := str(row["id"])
		seen[id]++
		for _, secret := range []string{"backend_url", "api_key", "authorization"} {
			if _, has := row[secret]; has {
				t.Fatalf("%s must never be exposed for %s", secret, id)
			}
		}
	}
	for _, id := range []string{"glm", "spark-auto", "flux2-klein-4b"} {
		if seen[id] != 1 {
			t.Fatalf("%s must appear exactly once, got %v", id, seen)
		}
	}

	// Loopback inference through the spark-auto route proxies to the backend.
	rec = serverRequest(t, s, "POST", "/v1/chat/completions", "127.0.0.1:5", `{"model":"spark-auto","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("inference failed: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("the backend reply must be proxied: %s", rec.Body.String())
	}

	// The image generation endpoint returns one base64 PNG.
	rec = serverRequest(t, s, "POST", "/v1/images/generations", "127.0.0.1:5", string(jsonBytes(Object{
		"model": "flux2-klein-4b", "prompt": "a small robot", "seed": json.Number("7"),
	})))
	if rec.Code != http.StatusOK {
		t.Fatalf("image generation failed: %d %s", rec.Code, rec.Body.String())
	}
	result := decodeResponse(t, rec.Body.String())
	if encoded := str(obj(arr(result["data"])[0])["b64_json"]); !strings.HasPrefix(encoded, "iVBOR") {
		t.Fatalf("no base64 PNG returned: %v", result)
	}
	env.engine.noUnexpected(t)
}

func TestImageSubmitOriginPinnedThroughGateAndEdit(t *testing.T) {
	env := newImageEnv(t, "native")
	// Engine A is the node's comfy_url; engine B is the selected setting's
	// backend_url. The submit must go to B and stay pinned on B.
	alt := newImageEngine(t)
	alt.setHistory(completedHistory("job-1", "success"))
	cfg := env.coord.Config()
	obj(obj(obj(obj(cfg["models"])["comfyui"])["settings"])["spark02"])["backend_url"] = alt.server.URL
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}

	// Hold the gate, then edit the config underneath the waiting request.
	env.coord.imageGate <- struct{}{}
	done := make(chan error, 1)
	go func() {
		_, err := env.generate(validImageBody())
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	edited := env.coord.Config()
	obj(edited["nodes"])["spark02"] = Object{"direct_ip": "10.0.0.2", "ssh": "spark02", "comfy_url": "http://127.0.0.1:1", "comfy_unit": "comfyui.service"}
	if err := writeCluster(env.path, edited); err != nil {
		t.Fatal(err)
	}
	<-env.coord.imageGate
	if err := <-done; err != nil {
		t.Fatalf("the request must succeed on the accepted origin: %v", err)
	}
	if prompts := env.engine.prompts(); len(prompts) != 0 {
		t.Fatal("the node comfy_url engine must never receive the submit")
	}
	env.engine.mu.Lock()
	hits, views := env.engine.histHits, env.engine.viewHits
	env.engine.mu.Unlock()
	if hits != 0 || views != 0 {
		t.Fatalf("history/view must stay on the accepted origin: %d %d", hits, views)
	}
	alt.noUnexpected(t)
}

func TestImageOutputRequiresActualStringKeys(t *testing.T) {
	env := newImageEnv(t, "native")
	cases := map[string]string{
		"missing type":      `{"job-1":{"status":{"completed":true,"status_str":"success"},"outputs":{"13":{"images":[{"filename":"a.png","subfolder":""}]}}}}`,
		"missing subfolder": `{"job-1":{"status":{"completed":true,"status_str":"success"},"outputs":{"13":{"images":[{"filename":"a.png","type":"output"}]}}}}`,
		"numeric subfolder": `{"job-1":{"status":{"completed":true,"status_str":"success"},"outputs":{"13":{"images":[{"filename":"a.png","subfolder":3,"type":"output"}]}}}}`,
		"null filename":     `{"job-1":{"status":{"completed":true,"status_str":"success"},"outputs":{"13":{"images":[{"subfolder":"","type":"output"}]}}}}`,
		"non string type":   `{"job-1":{"status":{"completed":true,"status_str":"success"},"outputs":{"13":{"images":[{"filename":"a.png","subfolder":"","type":1}]}}}}`,
		"empty filename":    `{"job-1":{"status":{"completed":true,"status_str":"success"},"outputs":{"13":{"images":[{"filename":"","subfolder":"","type":"output"}]}}}}`,
		"path in filename":  `{"job-1":{"status":{"completed":true,"status_str":"success"},"outputs":{"13":{"images":[{"filename":"../a.png","subfolder":"","type":"output"}]}}}}`,
	}
	for name, raw := range cases {
		env.engine.setHistory(raw)
		if _, err := env.generate(validImageBody()); err == nil {
			t.Fatalf("%s: a malformed output must be refused", name)
		}
	}
}

func TestImageUnsafePromptIDRejected(t *testing.T) {
	env := newImageEnv(t, "native")
	env.engine.setPromptID("../x?next=/history")
	if _, err := env.generate(validImageBody()); err == nil {
		t.Fatal("an unsafe prompt_id must be rejected")
	} else {
		var se statusError
		if !errors.As(err, &se) || se.HTTPStatus() != 409 {
			t.Fatalf("expected 409, got %v", err)
		}
	}
	env.engine.mu.Lock()
	hits := env.engine.histHits
	env.engine.mu.Unlock()
	if hits != 0 {
		t.Fatalf("an unsafe prompt_id must never be polled: %d history hits", hits)
	}
	if prompts := env.engine.prompts(); len(prompts) != 1 {
		t.Fatal("the accepted job must remain submitted")
	}
	env.engine.noUnexpected(t)
}

func TestImageSafePromptIDRejectsDotSegments(t *testing.T) {
	if safePromptID(".") || safePromptID("..") {
		t.Fatal("exact dot segments must be rejected: PathEscape never escapes them")
	}
	if !safePromptID("a..b") || !safePromptID("job_1") {
		t.Fatal("ordinary token ids containing dots or underscores must stay accepted")
	}
	env := newImageEnv(t, "native")
	env.engine.setPromptID("..")
	if _, err := env.generate(validImageBody()); err == nil {
		t.Fatal("a dot-segment prompt_id must be rejected")
	}
	env.engine.mu.Lock()
	hits := env.engine.histHits
	env.engine.mu.Unlock()
	if hits != 0 {
		t.Fatalf("a dot-segment prompt_id must never reach a history URL: %d hits", hits)
	}
	if prompts := env.engine.prompts(); len(prompts) != 1 {
		t.Fatal("the accepted job must be retained")
	}
}

// gateHeld starts a Generate while the test holds the gate, mutates the
// config once the request has passed pre-gate validation, then releases the
// gate. The caller restores imageGateWait.
func gateHeld(t *testing.T, env *imageEnv, mutate func(Object)) <-chan error {
	t.Helper()
	env.coord.imageGate <- struct{}{}
	done := make(chan error, 1)
	go func() {
		_, err := env.generate(validImageBody())
		done <- err
	}()
	// Pre-gate validation is pure CPU work over a local config; a bounded
	// sleep guarantees the request is parked on the gate before the edit.
	time.Sleep(100 * time.Millisecond)
	cfg, err := readCluster(env.path)
	if err != nil {
		t.Fatal(err)
	}
	mutate(cfg)
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	<-env.coord.imageGate
	return done
}

// TestImageRevalidatesAfterGate proves that an image_api disabled while the
// gate is held submits nothing: the post-gate revalidation sees the edit.
func TestImageRevalidatesAfterGate(t *testing.T) {
	env := newImageEnv(t, "native")
	prevWait := imageGateWait
	imageGateWait = 3 * time.Second
	defer func() { imageGateWait = prevWait }()
	done := gateHeld(t, env, func(cfg Object) {
		obj(obj(obj(cfg["models"])["flux2-klein-4b"])["image_api"])["enabled"] = false
	})
	assertStatus(t, <-done, 409, "no longer enabled")
	if prompts := env.engine.prompts(); len(prompts) != 0 {
		t.Fatal("a disabled image API after the gate must submit nothing")
	}
}

// TestImageUsesCurrentNodeAfterGateEdit proves a node edit while the gate is
// held submits to the newly validated node and origin, not the stale one.
func TestImageUsesCurrentNodeAfterGateEdit(t *testing.T) {
	env := newImageEnv(t, "native")
	second := newImageEngine(t)
	prevWait := imageGateWait
	imageGateWait = 3 * time.Second
	defer func() { imageGateWait = prevWait }()
	done := gateHeld(t, env, func(cfg Object) {
		api := obj(obj(obj(cfg["models"])["flux2-klein-4b"])["image_api"])
		api["node"] = "spark01"
		obj(obj(cfg["nodes"])["spark01"])["comfy_url"] = second.server.URL
		comfy := obj(obj(cfg["models"])["comfyui"])
		obj(obj(comfy)["settings"])["spark01"] = Object{"backend_url": second.server.URL, "units": Object{"spark01": "comfyui.service"}, "context_tokens": 1, "concurrency": 1}
	})
	// Complete the job on the second engine as soon as it arrives there.
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && len(second.prompts()) == 0 {
			time.Sleep(time.Millisecond)
		}
		second.setHistory(completedHistory("job-1", "success"))
	}()
	if err := <-done; err != nil {
		t.Fatalf("the request must succeed on the new node: %v", err)
	}
	if prompts := env.engine.prompts(); len(prompts) != 0 {
		t.Fatal("the stale node must not receive the job")
	}
	if prompts := second.prompts(); len(prompts) != 1 {
		t.Fatal("the revalidated node must receive exactly one job")
	}
}

// TestSubmitComfyNormalizesTrailingSlash proves a trailing slash on the
// configured engine origin is normalized before /prompt and /history.
func TestSubmitComfyNormalizesTrailingSlash(t *testing.T) {
	env := newImageEnv(t, "native")
	cfg, err := readCluster(env.path)
	if err != nil {
		t.Fatal(err)
	}
	obj(obj(obj(obj(cfg["models"])["comfyui"])["settings"])["spark02"])["backend_url"] = env.engine.server.URL + "/"
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	env.coord.refresh()
	if _, err := env.coord.SubmitComfy(imageGraph(), ""); err != nil {
		t.Fatalf("submission with a trailing-slash origin failed: %v", err)
	}
	if prompts := env.engine.prompts(); len(prompts) != 1 {
		t.Fatal("the engine must receive a /prompt request on the normalized origin")
	}
	env.engine.noUnexpected(t)
}
