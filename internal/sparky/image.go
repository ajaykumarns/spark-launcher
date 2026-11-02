package sparky

// OpenAI image adapter for the tested local ComfyUI workflow, mirroring
// src/spark_launcher/image_api.py: exactly the FLUX2-Klein 4B workflow13
// graph, strict input validation before any submission, one gateway image
// job across the cluster (bounded gate), polling of the accepted engine's
// history, and a signature/size-checked PNG view fetch. Accepted engine jobs
// are never cancelled: on timeout or failure the job stays queued and the
// gate is released so a human can inspect the engine queue.
//
// Privacy: events carry workload, node, prompt id, timing and dimensions
// only — never the prompt, request headers or upstream URLs.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// HTTPStatus errors distinguish client input errors (400) from resource or
// engine conflicts (409) at the server boundary without coupling the server
// to any concrete image type.
type statusError struct {
	status int
	err    error
}

func (e statusError) Error() string   { return e.err.Error() }
func (e statusError) HTTPStatus() int { return e.status }
func (e statusError) Unwrap() error   { return e.err }

func statusErr(status int, format string, args ...any) error {
	return statusError{status: status, err: fail(format, args...)}
}

const (
	imageModelName       = "flux2-klein-4b"
	imageMaxPromptRunes  = 8000
	imageMaxPNG          = 16 << 20
	imageWorkflowOutputs = "13"
)

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// Effect seams. Production values bound the gate wait (600s), the accepted
// job's history polling (900s) and the per-request engine timeouts; tests
// shrink them so no test ever waits on a real clock.
var (
	imageGateWait       = 600 * time.Second
	imagePollBudget     = 900 * time.Second
	imagePollInterval   = time.Second
	imageHistoryTimeout = 10 * time.Second
	imageViewTimeout    = 30 * time.Second
	imageWireBudget     = 15 * time.Second
)

var imageAllowedFields = strings.Fields("model prompt size n response_format seed stream quality user")

// fluxWorkflow is the exact tested FLUX2-Klein 4B graph: distilled fp8 UNET,
// four scheduler steps, batch size one, and the seed preserved as an exact
// unsigned 64-bit integer into the workflow JSON.
func fluxWorkflow(prompt string, width, height int, seed uint64) Object {
	node := func(kind string, inputs Object) Object {
		return Object{"class_type": kind, "inputs": inputs}
	}
	return Object{
		"1":  node("UNETLoader", Object{"unet_name": "flux-2-klein-4b-fp8.safetensors", "weight_dtype": "default"}),
		"2":  node("CLIPLoader", Object{"clip_name": "qwen_3_4b.safetensors", "type": "flux2", "device": "default"}),
		"3":  node("VAELoader", Object{"vae_name": "flux2-vae.safetensors"}),
		"4":  node("CLIPTextEncode", Object{"clip": []any{"2", 0}, "text": prompt}),
		"5":  node("ConditioningZeroOut", Object{"conditioning": []any{"4", 0}}),
		"6":  node("CFGGuider", Object{"model": []any{"1", 0}, "positive": []any{"4", 0}, "negative": []any{"5", 0}, "cfg": 1.0}),
		"7":  node("RandomNoise", Object{"noise_seed": seed}),
		"8":  node("KSamplerSelect", Object{"sampler_name": "euler"}),
		"9":  node("Flux2Scheduler", Object{"steps": 4, "width": width, "height": height}),
		"10": node("EmptyFlux2LatentImage", Object{"width": width, "height": height, "batch_size": 1}),
		"11": node("SamplerCustomAdvanced", Object{"noise": []any{"7", 0}, "guider": []any{"6", 0}, "sampler": []any{"8", 0}, "sigmas": []any{"9", 0}, "latent_image": []any{"10", 0}}),
		"12": node("VAEDecode", Object{"samples": []any{"11", 0}, "vae": []any{"3", 0}}),
		"13": node("SaveImage", Object{"images": []any{"12", 0}, "filename_prefix": "Sparky-FLUX2-Klein"}),
	}
}

// parseImageSeed accepts an exact unsigned 64-bit integer: typed Go integers
// within range or the exact JSON spelling. Booleans, fractions, negatives,
// floats and out-of-range values are rejected without any float conversion.
func parseImageSeed(v any) (uint64, error) {
	reject := func() (uint64, error) {
		return 0, statusErr(400, "Image seed must be an unsigned 64-bit integer.")
	}
	switch x := v.(type) {
	case uint64:
		return x, nil
	case int:
		if x < 0 {
			return reject()
		}
		return uint64(x), nil
	case int64:
		if x < 0 {
			return reject()
		}
		return uint64(x), nil
	case json.Number:
		u, err := strconv.ParseUint(x.String(), 10, 64)
		if err != nil {
			return reject()
		}
		return u, nil
	default:
		return reject()
	}
}

// imageSeed draws the default seed securely from 63 random bits.
func imageSeed() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fail("Secure randomness is unavailable.")
	}
	return binary.BigEndian.Uint64(b[:]) & (1<<63 - 1), nil
}

// lockImageGate admits one gateway image job across the whole cluster.
func (c *Coordinator) lockImageGate(ctx context.Context) error {
	select {
	case c.imageGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fail("The image gate did not open in time.")
	}
}

func (c *Coordinator) unlockImageGate() { <-c.imageGate }

// ImageModels lists image-capable models from the central configuration.
// It is a pure config projection: listing never starts, reloads or evicts
// anything, and it carries no backend URLs or credentials.
func (c *Coordinator) ImageModels() []any {
	cfg := c.cfg()
	out := []any{}
	for _, name := range keys(obj(cfg["models"])) {
		m := obj(obj(cfg["models"])[name])
		if !boolean(obj(m["image_api"])["enabled"]) {
			continue
		}
		out = append(out, Object{"id": name, "object": "model", "owned_by": "garuda",
			"input_modalities":    []any{"text"},
			"output_modalities":   []any{"image"},
			"supported_endpoints": []any{"/v1/images/generations"}})
	}
	return out
}

// LLMWire returns the Bifrost-required /v1/models listing. In legacy parity
// mode it is the installed manager's own listing, errors preserved verbatim.
// In native mode it is synthesized from the configuration: canonical
// unprefixed IDs, one row per enabled language model plus the router, each
// carrying a status object. Readiness comes from Runtime.ready only — the
// listing never starts, reloads or evicts a model, and no manager is called.
func (c *Coordinator) LLMWire() (Object, error) {
	if err := c.refresh(); err != nil {
		return nil, err
	}
	cfg := c.cfg()
	ctx, cancel := context.WithTimeout(context.Background(), imageWireBudget)
	defer cancel()
	if str(cfg["lifecycle"]) != "native" {
		return c.runtime.legacyManager(ctx, "/v1/models", "GET")
	}
	data := []any{}
	for _, name := range enabledLLMs(cfg) {
		_, s, nodes, _, err := selectPlacement(cfg, name, "")
		if err != nil {
			return nil, err
		}
		value := "unloaded"
		if c.runtime.ready(ctx, name, s, nodes) {
			value = "loaded"
		}
		data = append(data, Object{"id": name, "object": "model", "owned_by": "sparky",
			"status": Object{"value": value}})
	}
	if auto := obj(cfg["auto"]); len(auto) > 0 {
		value := "unloaded"
		for _, name := range stringsOf(auto["targets"]) {
			_, s, nodes, _, err := selectPlacement(cfg, name, "")
			if err != nil {
				continue
			}
			if c.runtime.ready(ctx, name, s, nodes) {
				value = "loaded"
				break
			}
		}
		data = append(data, Object{"id": "spark-auto", "object": "model", "owned_by": "sparky",
			"status": Object{"value": value}})
	}
	return Object{"object": "list", "data": data}, nil
}

// Generate runs one image request end to end. All input validation happens
// before any submission; the engine job is submitted under the cluster-wide
// image gate and polled on the accepted engine's own history.
func (c *Coordinator) Generate(body Object) (Object, error) {
	if err := c.refresh(); err != nil {
		return nil, err
	}
	cfg := c.cfg()
	name, m, err := resolveWorkload(cfg, str(body["model"]))
	if err != nil {
		return nil, statusErr(400, "%s", err.Error())
	}
	api := obj(m["image_api"])
	if name != imageModelName || !boolean(api["enabled"]) {
		return nil, statusErr(400, "This image model has no tested gateway workflow. Use %s.", imageModelName)
	}
	var unknown []string
	for _, k := range keys(body) {
		if !contains(imageAllowedFields, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return nil, statusErr(400, "Unsupported image fields: %s.", strings.Join(unknown, ", "))
	}
	prompt, isStr := body["prompt"].(string)
	if !isStr || strings.TrimSpace(prompt) == "" || utf8.RuneCountInString(prompt) > imageMaxPromptRunes {
		return nil, statusErr(400, "Give an image prompt of 1 to 8000 characters.")
	}
	if raw, ok := body["n"]; ok {
		n, nerr := toInt(raw)
		if !integer(raw) || nerr != nil || n != 1 {
			return nil, statusErr(400, "Generate one image per request.")
		}
	}
	if raw, ok := body["stream"]; ok {
		b, isBool := raw.(bool)
		if !isBool || b {
			return nil, statusErr(400, "Image streaming is not supported.")
		}
	}
	if raw, ok := body["quality"]; ok && raw != "auto" && raw != "standard" {
		return nil, statusErr(400, "Use auto or standard image quality.")
	}
	if raw, ok := body["response_format"]; ok && raw != "b64_json" {
		return nil, statusErr(400, "Use response_format b64_json.")
	}
	size, isStr := "1024x1024", true
	if raw, ok := body["size"]; ok {
		size, isStr = raw.(string)
	}
	if !isStr || !contains([]string{"512x512", "768x768", "1024x1024"}, size) {
		return nil, statusErr(400, "Use 512x512, 768x768, or 1024x1024.")
	}
	width, height := 1024, 1024
	if size != "1024x1024" {
		a := strings.SplitN(size, "x", 2)
		width, _ = strconv.Atoi(a[0])
		height, _ = strconv.Atoi(a[1])
	}
	var seed uint64
	if raw, ok := body["seed"]; ok {
		seed, err = parseImageSeed(raw)
		if err != nil {
			return nil, err
		}
	} else {
		if seed, err = imageSeed(); err != nil {
			return nil, err
		}
	}
	on := str(api["node"])
	if on == "" {
		on = str(m["run_on"])
	}

	// One gateway image job across the cluster, bounded wait. Active engine
	// work is never interrupted while waiting. After acquiring the gate the
	// image model, API enablement and target node are revalidated from ONE
	// immutable config snapshot that is also used for the actual submission:
	// a disabled API or changed node while waiting never submits a stale
	// selection. The accepted engine's origin is held from submit through
	// poll and view.
	gateCtx, gateCancel := context.WithTimeout(context.Background(), imageGateWait)
	defer gateCancel()
	if err := c.lockImageGate(gateCtx); err != nil {
		return nil, statusErr(409, "The image queue is busy. Retry later.")
	}
	defer c.unlockImageGate()

	if err := c.refresh(); err != nil {
		return nil, err
	}
	gateCfg := c.cfg()
	if name2, m2, err := resolveWorkload(gateCfg, str(body["model"])); err != nil || name2 != imageModelName || !boolean(obj(m2["image_api"])["enabled"]) {
		return nil, statusErr(409, "The image API is no longer enabled. Nothing was submitted.")
	}
	api2 := obj(obj(obj(gateCfg["models"])[imageModelName])["image_api"])
	on = str(api2["node"])
	if on == "" {
		on = str(obj(obj(gateCfg["models"])[imageModelName])["run_on"])
	}

	started := time.Now()
	comfyURL, submitted, err := c.submitComfyPinned(gateCfg, on, fluxWorkflow(prompt, width, height, seed))
	if err != nil {
		return nil, statusErr(409, "%s", err.Error())
	}
	comfyURL = strings.TrimRight(comfyURL, "/")
	promptID := str(submitted["prompt_id"])
	if !safePromptID(promptID) {
		// The job is accepted; an unsafe id must simply never reach a URL.
		return nil, statusErr(409, "The ComfyUI engine returned an unsafe prompt_id.")
	}
	deadline := time.Now().Add(imagePollBudget)
	for {
		if !time.Now().Before(deadline) {
			// The accepted job is kept: nothing is cancelled, the caller may
			// inspect the engine queue before retrying.
			return nil, statusErr(409, "The image request timed out. The accepted ComfyUI job was kept; check its queue before retrying.")
		}
		history, err := requestObject(context.Background(), "GET", comfyURL+"/history/"+url.PathEscape(promptID), nil, nil, imageHistoryTimeout, false)
		if err == nil {
			entry := obj(history[promptID])
			if len(entry) > 0 {
				status := obj(entry["status"])
				if str(status["status_str"]) == "error" {
					return nil, statusErr(409, "The image workflow failed. Check the ComfyUI job history.")
				}
				if boolean(status["completed"]) {
					return c.imageOutput(comfyURL, entry, name, on, promptID, width, height, seed, started)
				}
			}
		}
		time.Sleep(imagePollInterval)
	}
}

// safePromptID accepts a nonempty path-token prompt id: plain word
// characters only, so it can never smuggle "../query" fragments into the
// history URL. It is additionally URL-escaped at every use.
func safePromptID(id string) bool {
	// "." and ".." are exact dot segments: PathEscape does not escape them,
	// so they must be rejected before any history fetch.
	if id == "." || id == ".." {
		return false
	}
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// imageOutput validates the saved image of the completed job: only the fixed
// engine origin with URL-encoded query parameters is fetched, the response
// must be a PNG of at most 16 MiB, and only metadata is recorded.
func (c *Coordinator) imageOutput(comfyURL string, entry Object, name, on, promptID string, width, height int, seed uint64, started time.Time) (Object, error) {
	invalid := statusErr(409, "Invalid image output.")
	images := arr(obj(obj(entry["outputs"])[imageWorkflowOutputs])["images"])
	if len(images) == 0 {
		return nil, statusErr(409, "The image workflow returned no saved image.")
	}
	item := obj(images[0])
	// filename, subfolder and type must be actual string keys; a missing or
	// non-string field is never silently coerced into an empty value.
	filename, okF := item["filename"].(string)
	subfolder, okS := item["subfolder"].(string)
	kind, okT := item["type"].(string)
	if !okF || !okS || !okT || filename == "" || kind == "" ||
		strings.ContainsAny(filename, `/\`) {
		return nil, invalid
	}
	base, err := url.Parse(comfyURL)
	if err != nil || base.Host == "" || base.User != nil {
		return nil, invalid
	}
	view := *base
	view.Path = "/view"
	view.RawQuery = url.Values{"filename": {filename}, "subfolder": {subfolder}, "type": {kind}}.Encode()
	view.Fragment = ""
	png, err := imageViewFetch(view.String(), imageViewTimeout)
	if err != nil || !bytes.HasPrefix(png, pngSignature) || len(png) > imageMaxPNG {
		return nil, invalid
	}
	_ = c.runtime.event("image_api_completed", Object{"workload": name, "node": on,
		"prompt_id": promptID, "seconds": time.Since(started).Seconds(),
		"width": width, "height": height, "seed": seed})
	return Object{"created": time.Now().Unix(),
		"data": []any{Object{"b64_json": base64.StdEncoding.EncodeToString(png)}}}, nil
}

// imageViewFetch reads at most 16 MiB + 1 bytes so an oversize response is
// detected by length, never truncated into a silently wrong image.
func imageViewFetch(viewURL string, timeout time.Duration) ([]byte, error) {
	resp, err := httpClient(timeout).Get(viewURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fail("The image view request failed.")
	}
	return io.ReadAll(io.LimitReader(resp.Body, imageMaxPNG+1))
}

// The Coordinator completes the Controller interface; ServeController wires
// the real implementation through CoordinatorFactory.
var _ Controller = (*Coordinator)(nil)

func init() {
	CoordinatorFactory = func(configPath string) (Controller, error) {
		return newCoordinator(configPath, nil)
	}
}
