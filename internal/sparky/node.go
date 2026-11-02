package sparky

// Private node protocol used by the manager and the SSH client. The output
// stays JSON; no human rendering happens here. NodeMain receives the argv
// after the "node" command word and mirrors src/spark_launcher/cli.py
// node_action exactly, including JSON return shapes.
//
// Supervisor hook pattern (native lifecycle):
//
//	sparky --json node supervise  --file CONFIG --model NAME
//	sparky --json node lifecycle  --file CONFIG --model NAME --action stop|cleanup
//
// CONFIG is the cluster config file path (readCluster takes it directly).
// supervise runs the model for its whole lifetime and reacts to SIGINT and
// SIGTERM; a signal during partial startup cleans up the started ranks. It
// deliberately has no fixed timeout. lifecycle stop/cleanup is bounded by the
// runtime's own timeouts.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// controllerSeam replaces the loopback controller HTTP call in tests; nil in
// production, where controllerRequest always talks to 127.0.0.1.
var controllerSeam func(context.Context, string, any) (Object, error)

// parseNodeFlags parses "--name value" style flags strictly. Unknown flags,
// duplicate flags, missing values, and unexpected positional arguments are
// rejected. Booleans are declared explicitly. positionals allows exactly the
// given count of positional arguments (0 for most actions, 1 for rpc).
type nodeFlags struct {
	values map[string]string
	set    map[string]bool
}

func parseNodeFlags(args []string, allowed map[string]bool, boolean map[string]bool, positionals int) (*nodeFlags, []string, error) {
	f := &nodeFlags{values: map[string]string{}, set: map[string]bool{}}
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		if !allowed[name] {
			return nil, nil, usagef("Unknown option --%s.", name)
		}
		if f.set[name] {
			return nil, nil, usagef("Option --%s was given more than once.", name)
		}
		if boolean[name] {
			f.values[name] = "true"
			f.set[name] = true
			continue
		}
		if i+1 >= len(args) {
			return nil, nil, usagef("Option --%s needs a value.", name)
		}
		i++
		f.values[name] = args[i]
		f.set[name] = true
	}
	if len(positional) != positionals {
		if positionals == 0 {
			return nil, nil, usagef("This action does not take positional arguments.")
		}
		return nil, nil, usagef("Choose exactly one operation.")
	}
	return f, positional, nil
}

func (f *nodeFlags) get(name, fallback string) string {
	if !f.set[name] {
		return fallback
	}
	return f.values[name]
}

func (f *nodeFlags) require(name string) (string, error) {
	if !f.set[name] || strings.TrimSpace(f.values[name]) == "" {
		return "", usagef("Option --%s is required.", name)
	}
	return f.values[name], nil
}

// nodePayloadMaxBytes bounds any JSON payload read from stdin or a file at
// 64 MiB: the known heaviest legitimate use is the write-file transfer whose
// base64-encoded payload can reach ~24 MiB, with generous headroom. The
// bound is enforced while reading so an oversized stream is never fully
// allocated.
const nodePayloadMaxBytes = 64 << 20

// Binding: the private node dispatcher is the real default. Tests may save
// and restore the NodeDispatch seam variable itself.
func init() { NodeDispatch = NodeMain }

// nodePayload parses a --payload value: literal JSON, or "-" for stdin.
func nodePayload(in io.Reader, spec string) (Object, error) {
	var data []byte
	if spec == "-" {
		bounded, err := io.ReadAll(io.LimitReader(in, nodePayloadMaxBytes+1))
		if err != nil {
			return nil, fail("Could not read the JSON payload from standard input.")
		}
		if len(bounded) > nodePayloadMaxBytes {
			return nil, fail("The JSON payload exceeds %d MiB.", nodePayloadMaxBytes>>20)
		}
		data = bounded
	} else {
		data = []byte(spec)
	}
	payload, err := decode(data)
	if err != nil {
		return nil, fail("The payload is not a valid JSON object.")
	}
	return payload, nil
}

// nodePayloadFile reads JSON from a --file path, or stdin for "-".
func nodePayloadFile(in io.Reader, path string) (Object, error) {
	if path == "-" {
		return nodePayload(in, "-")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fail("Could not read %s.", path)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, nodePayloadMaxBytes+1))
	if cerr := file.Close(); readErr != nil || cerr != nil {
		return nil, fail("Could not read %s.", path)
	}
	if len(raw) > nodePayloadMaxBytes {
		return nil, fail("The file %s exceeds %d MiB.", path, nodePayloadMaxBytes>>20)
	}
	payload, err := decode(raw)
	if err != nil {
		return nil, fail("The file %s is not a valid JSON object.", path)
	}
	return payload, nil
}

// controllerRequest calls the localhost controller. It mirrors control.py
// controller_request: configuration comes from the cluster file and load and
// unload use the long timeout. controllerSeam is a test seam that replaces
// the loopback HTTP call; nil in production.
func controllerRequest(ctx context.Context, root, operation string, payload any) (Object, error) {
	if !simpleName.MatchString(operation) {
		return nil, fail("Unknown controller operation.")
	}
	cfg, err := readCluster(filepath.Join(root, "config", "models.json"))
	if err != nil {
		return nil, err
	}
	if controllerSeam != nil {
		return controllerSeam(ctx, operation, payload)
	}
	port := controllerPort(cfg)
	if port == 0 {
		return nil, fail("The cluster file has no usable controller port.")
	}
	timeout := 120 * time.Second
	if operation == "load" || operation == "unload" {
		timeout = 7200 * time.Second
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/control/%s", port, operation)
	method := "GET"
	if payload != nil {
		method = "POST"
	}
	return requestObject(ctx, method, url, payload, nil, timeout, true)
}

// nodeResources mirrors control.py node_resources for status, load, unload.
func nodeResources(ctx context.Context, root, action, model string, force bool, on ...string) (Object, error) {
	cfg, err := readCluster(filepath.Join(root, "config", "models.json"))
	if err != nil {
		return nil, err
	}
	override := ""
	if len(on) > 0 {
		override = on[0]
	}
	switch action {
	case "status":
		document, err := controllerRequest(ctx, root, "status", nil)
		if err != nil {
			return nil, err
		}
		return EnrichStatus(cfg, document)
	case "load", "unload":
		name, _, err := resolveWorkload(cfg, model)
		if err != nil {
			return nil, err
		}
		// An explicit placement must be validated against the selected
		// model's ACTUAL recipe BEFORE the controller changes anything.
		if override != "" {
			if action != "load" || placements[override] == nil {
				return nil, fail("--on is available only for load and must be spark01, spark02, or both.")
			}
			// Legacy deployments have no native lifecycle: a one-shot
			// placement override is refused before any controller write,
			// exactly like plan (the manager sync/HUP path would ignore it).
			if str(cfg["lifecycle"]) != "native" {
				return nil, fail("--on requires the native model lifecycle and cannot be used in legacy mode.")
			}
			if _, _, _, _, err := selectPlacement(cfg, name, override); err != nil {
				return nil, err
			}
			// Mixed-version safety: an updated node talking to an OLD still
			// running controller would have its "on" silently ignored while
			// the saved placement loads. The READ-ONLY plan must advertise
			// the override capability before the load (or force) is sent.
			plan, err := controllerRequest(ctx, root, "plan", Object{"action": action, "model": name, "on": override})
			if err != nil {
				return nil, err
			}
			if raw, has := plan["placement_override_supported"]; !has || raw != true {
				return nil, fail("The controller does not support one-shot placement overrides. Restart the updated controller on the machine and try again.")
			}
		}
		payload := Object{"model": name}
		if force {
			payload["force"] = true
		}
		if override != "" {
			payload["on"] = override
		}
		result, err := controllerRequest(ctx, root, action, payload)
		if err != nil {
			return nil, err
		}
		if override != "" {
			// A one-shot load acknowledges ONLY what the controller confirmed
			// from the durable record: a missing or contradictory placement is
			// an old/ignoring controller, never a successful override.
			if str(result["placement"]) != override || !sameStrings(stringsOf(result["nodes"]), placements[override]) {
				return nil, fail("The controller did not confirm the requested placement %s for %s. The controller may need an upgrade and restart; no placement change is reported.", override, name)
			}
		}
		return result, nil
	}
	return nil, fail("Unknown resource operation.")
}

// signalContext is a cancellable context that reacts to SIGINT and SIGTERM so
// a partially started model can clean up its ranks.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// NodeMain runs one private node action. args are the tokens after "node".
// The first token is the action; remaining flags are parsed strictly per
// action so an unknown or malformed option fails loudly before any effect.
func NodeMain(args []string, in io.Reader) (any, error) {
	if len(args) == 0 {
		return nil, usagef("Choose a node action.")
	}
	action, rest := args[0], args[1:]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	switch action {
	case "device":
		if _, _, err := parseNodeFlags(rest, map[string]bool{}, map[string]bool{}, 0); err != nil {
			return nil, err
		}
		return Device()

	case "memory":
		if _, _, err := parseNodeFlags(rest, map[string]bool{}, map[string]bool{}, 0); err != nil {
			return nil, err
		}
		spec, err := nodePayload(in, "-")
		if err != nil {
			return nil, err
		}
		return CollectMemory(spec)

	case "ocr":
		flags, _, err := parseNodeFlags(rest, map[string]bool{"root": true, "payload": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		root, err := flags.require("root")
		if err != nil {
			return nil, err
		}
		payload, err := nodePayload(in, flags.get("payload", "-"))
		if err != nil {
			return nil, err
		}
		return ManageOCR(root, payload)

	case "install":
		flags, _, err := parseNodeFlags(rest, map[string]bool{"dir": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		dir, err := flags.require("dir")
		if err != nil {
			return nil, err
		}
		return InstallHead(dir)

	case "drain-comfy":
		flags, _, err := parseNodeFlags(rest, map[string]bool{"file": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		file, err := flags.require("file")
		if err != nil {
			return nil, err
		}
		if err := DrainComfy(file); err != nil {
			return nil, err
		}
		return Object{"status": "drained"}, nil

	case "probe":
		flags, _, err := parseNodeFlags(rest, map[string]bool{"file": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		file, err := flags.require("file")
		if err != nil {
			return nil, err
		}
		node, err := nodePayloadFile(in, file)
		if err != nil {
			return nil, err
		}
		return Probe(node)

	case "write-file":
		// Private atomic transfer receiver for the deployment foundation:
		// exactly one JSON object with path, mode, and base64 payload on
		// standard input. No payload or secret ever appears on argv.
		if _, _, err := parseNodeFlags(rest, map[string]bool{}, map[string]bool{}, 0); err != nil {
			return nil, err
		}
		return nodeWriteFile(in)

	case "controller":
		flags, _, err := parseNodeFlags(rest, map[string]bool{"file": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		file, err := flags.require("file")
		if err != nil {
			return nil, err
		}
		if err := ServeController(file); err != nil {
			return nil, err
		}
		return nil, nil

	case "render":
		flags, _, err := parseNodeFlags(rest, map[string]bool{"payload": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		payloadFlag, err := flags.require("payload")
		if err != nil {
			return nil, err
		}
		spec, err := nodePayload(in, payloadFlag)
		if err != nil {
			return nil, err
		}
		return RenderParameters(spec)

	case "rpc":
		flags, positional, err := parseNodeFlags(rest,
			map[string]bool{"root": true, "payload": true}, map[string]bool{}, 1)
		if err != nil {
			return nil, err
		}
		root, err := flags.require("root")
		if err != nil {
			return nil, err
		}
		operation := positional[0]
		switch operation {
		case "plan", "config", "placement", "run", "validate", "image-profile":
		default:
			return nil, usagef("Unknown rpc operation: %s.", operation)
		}
		payload, err := nodePayload(in, flags.get("payload", "{}"))
		if err != nil {
			return nil, err
		}
		return nodeRPC(ctx, root, operation, payload)

	case "status":
		flags, _, err := parseNodeFlags(rest, map[string]bool{"root": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		root, err := flags.require("root")
		if err != nil {
			return nil, err
		}
		return nodeResources(ctx, root, "status", "", false)

	case "gateway":
		flags, _, err := parseNodeFlags(rest, map[string]bool{"root": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		root, err := flags.require("root")
		if err != nil {
			return nil, err
		}
		return EnableGateway(root)

	case "load", "unload":
		flags, _, err := parseNodeFlags(rest,
			map[string]bool{"root": true, "model": true, "force": true, "on": true}, map[string]bool{"force": true}, 0)
		if err != nil {
			return nil, err
		}
		root, err := flags.require("root")
		if err != nil {
			return nil, err
		}
		model, err := flags.require("model")
		if err != nil {
			return nil, err
		}
		on := flags.get("on", "")
		if on != "" && (action != "load" || placements[on] == nil) {
			return nil, usagef("--on is available only for load and must be spark01, spark02, or both.")
		}
		return nodeResources(ctx, root, action, model, flags.set["force"], on)

	case "supervise":
		flags, _, err := parseNodeFlags(rest, map[string]bool{"file": true, "model": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		file, err := flags.require("file")
		if err != nil {
			return nil, err
		}
		model, err := flags.require("model")
		if err != nil {
			return nil, err
		}
		if !simpleName.MatchString(model) {
			return nil, usagef("Invalid model name.")
		}
		// superviseModel blocks for the whole model lifetime; a SIGINT or
		// SIGTERM cancels the context so partially started ranks are cleaned
		// up. There is deliberately no fixed timeout here.
		supervisorCtx, stopSignals := signalContext()
		defer stopSignals()
		if err := superviseModel(supervisorCtx, file, "run", model); err != nil {
			return nil, err
		}
		return Object{"model": model, "supervisor": "run", "state": "ok"}, nil

	case "lifecycle":
		flags, _, err := parseNodeFlags(rest,
			map[string]bool{"file": true, "model": true, "action": true}, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		file, err := flags.require("file")
		if err != nil {
			return nil, err
		}
		model, err := flags.require("model")
		if err != nil {
			return nil, err
		}
		if !simpleName.MatchString(model) {
			return nil, usagef("Invalid model name.")
		}
		supervisorAction, err := flags.require("action")
		if err != nil {
			return nil, err
		}
		switch supervisorAction {
		case "stop", "cleanup":
		default:
			return nil, usagef("Use stop or cleanup for the lifecycle action.")
		}
		if err := superviseModel(ctx, file, supervisorAction, model); err != nil {
			return nil, err
		}
		return Object{"model": model, "supervisor": supervisorAction, "state": "ok"}, nil
	}
	return nil, usagef("Unknown node action: %s.", action)
}

// nodeRPC mirrors the rpc branch of cli.py node_action.
func nodeRPC(ctx context.Context, root, operation string, payload Object) (any, error) {
	if operation == "image-profile" {
		mode := str(payload["mode"])
		if mode == "" {
			return nil, fail("The image-profile payload needs a mode.")
		}
		return ActivateImageProfile(root, mode, boolean(payload["dry_run"]))
	}
	if operation == "config" || operation == "validate" {
		document, err := controllerRequest(ctx, root, "config", nil)
		if err != nil {
			return nil, err
		}
		if operation == "validate" {
			return Object{"valid": true, "path": document["path"], "model_count": len(obj(obj(document["config"])["models"]))}, nil
		}
		if str(payload["model"]) != "" {
			cfg := obj(document["config"])
			name, model, err := resolveWorkload(cfg, str(payload["model"]))
			if err != nil {
				return nil, err
			}
			return Object{"path": document["path"], "model": name, "config": model}, nil
		}
		return document, nil
	}
	if str(payload["model"]) != "" {
		cfg, err := readCluster(filepath.Join(root, "config", "models.json"))
		if err != nil {
			return nil, err
		}
		name, _, err := resolveWorkload(cfg, str(payload["model"]))
		if err != nil {
			return nil, err
		}
		payload["model"] = name
		if operation == "placement" || operation == "plan" {
			if _, _, _, _, err := selectPlacement(cfg, name, str(payload["on"])); err != nil {
				return nil, err
			}
		}
	}
	result, err := controllerRequest(ctx, root, operation, payload)
	if err != nil {
		return nil, err
	}
	if operation == "plan" {
		cfg, err := readCluster(filepath.Join(root, "config", "models.json"))
		if err != nil {
			return nil, err
		}
		model := obj(obj(cfg["models"])[str(result["workload"])])
		recipeState := str(obj(obj(model["settings"])[str(result["placement"])])["recipe_validation"])
		if recipeState != "" {
			result["recipe_validation"] = recipeState
		}
	}
	return result, nil
}
